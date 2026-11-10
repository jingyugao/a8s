package artifacts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
)

const (
	ManifestFile           = "a8s-package.json"
	FormatVersion          = "a8s.package/v1"
	ArtifactType           = "application/vnd.a8s.package.v1"
	ConfigMediaType        = "application/vnd.a8s.package.config.v1+json"
	LayerMediaType         = "application/vnd.a8s.package.content.v1.tar"
	MaxFiles               = 20000
	MaxFileBytes     int64 = 128 << 20
	MaxPackageBytes  int64 = 512 << 20
	MaxMetadataBytes int64 = 4 << 20
)

// Manifest selects package content explicitly. It contains no instance,
// Workspace, sandbox image, credentials, or build commands.
type Manifest struct {
	SchemaVersion string       `json:"schemaVersion"`
	Name          string       `json:"name"`
	Version       string       `json:"version,omitempty"`
	Instructions  string       `json:"instructions"`
	Contents      []string     `json:"contents"`
	Skills        []Skill      `json:"skills,omitempty"`
	CLIs          []CLI        `json:"clis,omitempty"`
	Requirements  Requirements `json:"requirements,omitempty"`
}

type Skill struct {
	Name string `json:"name"`
	// Path is a directory containing SKILL.md and any referenced assets.
	Path string `json:"path"`
}

type CLI struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// Interpreter is empty for a native executable; otherwise the runtime
	// must also be declared in Requirements.Runtimes.
	Interpreter string `json:"interpreter,omitempty"`
}

type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
}

type Requirements struct {
	Platforms      []Platform        `json:"platforms,omitempty"`
	Runtimes       map[string]string `json:"runtimes,omitempty"`
	System         []string          `json:"system,omitempty"`
	AgentcProtocol string            `json:"agentcProtocol,omitempty"`
	Capabilities   []string          `json:"capabilities,omitempty"`
}

type File struct {
	Path       string `json:"path"`
	Digest     string `json:"digest"`
	Size       int64  `json:"size"`
	Executable bool   `json:"executable,omitempty"`
}

// Metadata is the verified OCI config. Files describes the selected content,
// excluding ManifestFile, whose canonical representation is stored in the tar.
type Metadata struct {
	Manifest Manifest `json:"manifest"`
	Files    []File   `json:"files"`
}

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

func packagePath(p string) error {
	if !fs.ValidPath(p) || p == "." || len(p) > 1024 || strings.ContainsAny(p, "\\:\x00") {
		return fmt.Errorf("invalid package path %q", p)
	}
	for _, part := range strings.Split(p, "/") {
		if len(part) > 255 {
			return fmt.Errorf("package path component too long: %q", p)
		}
	}
	return nil
}

func contentPath(p string) error {
	if err := packagePath(p); err != nil {
		return err
	}
	if p == ManifestFile {
		return fmt.Errorf("%s is reserved for package metadata", p)
	}
	for _, part := range strings.Split(strings.ToLower(p), "/") {
		if part == ".env" || strings.HasPrefix(part, ".env.") || part == ".git" || part == ".ssh" || part == ".aws" || part == ".kube" || part == ".docker" || part == ".npmrc" || part == ".pypirc" || part == ".netrc" || part == "auth.json" || part == "credentials" || part == "credentials.json" || part == "application_default_credentials.json" || part == "kubeconfig" || strings.HasSuffix(part, ".kubeconfig") || part == "id_rsa" || part == "id_ed25519" || strings.HasSuffix(part, ".key") || strings.HasSuffix(part, ".p12") || strings.HasSuffix(part, ".pfx") {
			return fmt.Errorf("explicit secret or private configuration path forbidden: %q", p)
		}
	}
	return nil
}

func decodeJSON(data []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return errors.New("expected exactly one JSON value")
	}
	return nil
}

func validateManifest(m Manifest) error {
	encoded, _ := json.Marshal(m)
	if bytes.Contains(encoded, []byte("PRIVATE KEY-----")) {
		return errors.New("private-key content forbidden in package metadata")
	}
	if m.SchemaVersion != FormatVersion {
		return fmt.Errorf("unsupported package format %q", m.SchemaVersion)
	}
	if !namePattern.MatchString(m.Name) {
		return fmt.Errorf("invalid package name %q", m.Name)
	}
	if err := contentPath(m.Instructions); err != nil {
		return err
	}
	if len(m.Contents) == 0 || len(m.Contents) > MaxFiles {
		return errors.New("contents must select at least one file or directory, within the file limit")
	}
	for i, p := range m.Contents {
		if err := contentPath(p); err != nil {
			return err
		}
		if slices.Contains(m.Contents[:i], p) {
			return fmt.Errorf("duplicate content selection %q", p)
		}
	}
	names := map[string]bool{}
	for _, skill := range m.Skills {
		if !namePattern.MatchString(skill.Name) || names[skill.Name] {
			return fmt.Errorf("invalid or duplicate skill name %q", skill.Name)
		}
		names[skill.Name] = true
		if err := contentPath(skill.Path); err != nil {
			return err
		}
	}
	names = map[string]bool{}
	for _, cli := range m.CLIs {
		if !namePattern.MatchString(cli.Name) || names[cli.Name] {
			return fmt.Errorf("invalid or duplicate CLI name %q", cli.Name)
		}
		names[cli.Name] = true
		if err := contentPath(cli.Path); err != nil {
			return err
		}
		if cli.Interpreter != "" && (!namePattern.MatchString(cli.Interpreter) || m.Requirements.Runtimes[cli.Interpreter] == "") {
			return fmt.Errorf("CLI %q must declare its interpreter in requirements.runtimes", cli.Name)
		}
	}
	for _, p := range m.Requirements.Platforms {
		if !namePattern.MatchString(p.OS) || !namePattern.MatchString(p.Architecture) {
			return errors.New("platform requires an OS and architecture")
		}
	}
	for runtime, version := range m.Requirements.Runtimes {
		if !namePattern.MatchString(runtime) || strings.TrimSpace(version) == "" {
			return fmt.Errorf("invalid runtime requirement %q", runtime)
		}
	}
	return nil
}

func validateMetadata(m Metadata) error {
	if err := validateManifest(m.Manifest); err != nil {
		return err
	}
	if len(m.Files) == 0 || len(m.Files) > MaxFiles {
		return errors.New("invalid package file count")
	}
	files := make(map[string]File, len(m.Files))
	selections := make(map[string]bool, len(m.Manifest.Contents))
	for _, p := range m.Manifest.Contents {
		selections[p] = false
	}
	var total int64
	for _, f := range m.Files {
		if err := contentPath(f.Path); err != nil {
			return err
		}
		if _, exists := files[f.Path]; exists {
			return fmt.Errorf("duplicate file %q", f.Path)
		}
		if err := validateDigest(f.Digest); err != nil {
			return fmt.Errorf("file %q: %w", f.Path, err)
		}
		if f.Size < 0 || f.Size > MaxFileBytes {
			return fmt.Errorf("file %q exceeds size limit", f.Path)
		}
		total += f.Size
		if total > MaxPackageBytes {
			return errors.New("package exceeds total content size limit")
		}
		selected := false
		for p := f.Path; p != "."; p = path.Dir(p) {
			if _, ok := selections[p]; ok {
				selected = true
				selections[p] = true
			}
		}
		if !selected {
			return fmt.Errorf("file %q is outside declared contents", f.Path)
		}
		files[f.Path] = f
	}
	for p, found := range selections {
		if !found {
			return fmt.Errorf("content selection %q contains no files", p)
		}
	}
	if _, ok := files[m.Manifest.Instructions]; !ok {
		return fmt.Errorf("instructions %q are not included", m.Manifest.Instructions)
	}
	for _, s := range m.Manifest.Skills {
		if _, ok := files[path.Join(s.Path, "SKILL.md")]; !ok {
			return fmt.Errorf("skill %q is missing SKILL.md", s.Name)
		}
	}
	for _, cli := range m.Manifest.CLIs {
		f, ok := files[cli.Path]
		if !ok {
			return fmt.Errorf("CLI %q entry %q is not included", cli.Name, cli.Path)
		}
		if cli.Interpreter == "" && !f.Executable {
			return fmt.Errorf("CLI %q is not executable and has no interpreter", cli.Name)
		}
	}
	return nil
}

func validateDigest(value string) error {
	if !strings.HasPrefix(value, "sha256:") || len(value) != 71 {
		return fmt.Errorf("expected sha256 digest, got %q", value)
	}
	_, err := hex.DecodeString(value[7:])
	if err != nil || value != strings.ToLower(value) {
		return fmt.Errorf("invalid sha256 digest %q", value)
	}
	return nil
}

func digestBytes(data []byte) string { h := sha256.Sum256(data); return fmt.Sprintf("sha256:%x", h) }

// copyContent checks cancellation, bounds file size, and rejects private-key
// content without including any content bytes in errors.
func copyContent(ctx context.Context, dst io.Writer, src io.Reader, limit int64) (int64, error) {
	buf := make([]byte, 32<<10)
	var previous []byte
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, err := src.Read(buf)
		if n > 0 {
			total += int64(n)
			if total > limit {
				return total, errors.New("content exceeds size limit")
			}
			window := append(previous, buf[:n]...)
			if bytes.Contains(window, []byte("PRIVATE KEY-----")) {
				return total, errors.New("private-key content forbidden")
			}
			previous = append([]byte(nil), window[max(0, len(window)-64):]...)
			if _, e := dst.Write(buf[:n]); e != nil {
				return total, e
			}
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

// Inspect validates a local source directory and hashes only explicitly selected
// files. Symlinks, special files, known credential files and private keys fail.
// The source is opened through os.Root so path resolution cannot escape it.
func Inspect(ctx context.Context, directory string) (Metadata, error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return Metadata{}, err
	}
	defer root.Close()
	info, err := root.Lstat(ManifestFile)
	if err != nil {
		return Metadata{}, err
	}
	if !info.Mode().IsRegular() {
		return Metadata{}, errors.New("package manifest must be a regular file")
	}
	f, err := root.Open(ManifestFile)
	if err != nil {
		return Metadata{}, err
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxMetadataBytes+1))
	f.Close()
	if err != nil {
		return Metadata{}, err
	}
	if int64(len(data)) > MaxMetadataBytes {
		return Metadata{}, errors.New("package manifest exceeds size limit")
	}
	var m Metadata
	if err := decodeJSON(data, &m.Manifest); err != nil {
		return m, fmt.Errorf("%s: %w", ManifestFile, err)
	}
	if err := validateManifest(m.Manifest); err != nil {
		return m, err
	}
	seen := map[string]bool{}
	var total int64
	for _, selection := range m.Manifest.Contents {
		err := fs.WalkDir(root.FS(), selection, func(p string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := contentPath(p); err != nil {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("symlinks are not supported: %q", p)
			}
			if entry.IsDir() {
				return nil
			}
			if seen[p] {
				return nil
			}
			seen[p] = true
			if len(seen) > MaxFiles {
				return errors.New("package exceeds file count limit")
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("not a regular file: %q", p)
			}
			if info.Size() > MaxFileBytes {
				return fmt.Errorf("file %q exceeds size limit", p)
			}
			file, err := root.Open(p)
			if err != nil {
				return err
			}
			defer file.Close()
			h := sha256.New()
			n, err := copyContent(ctx, h, file, MaxFileBytes)
			if err != nil {
				return fmt.Errorf("file %q: %w", p, err)
			}
			total += n
			if total > MaxPackageBytes {
				return errors.New("package exceeds total content size limit")
			}
			m.Files = append(m.Files, File{Path: p, Digest: fmt.Sprintf("sha256:%x", h.Sum(nil)), Size: n, Executable: info.Mode()&0111 != 0})
			return nil
		})
		if err != nil {
			return m, err
		}
	}
	slices.SortFunc(m.Files, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	return m, validateMetadata(m)
}

func Validate(ctx context.Context, directory string) error {
	_, err := Inspect(ctx, directory)
	return err
}
