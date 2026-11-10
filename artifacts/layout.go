package artifacts

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"
)

const layoutTag = "package"
const maxArchiveBytes = MaxPackageBytes + MaxFiles*2048 + MaxMetadataBytes + (1 << 20)

type Package struct {
	Reference string   `json:"reference,omitempty"`
	Digest    string   `json:"digest"`
	Metadata  Metadata `json:"metadata"`
}

type BuildResult struct {
	Layout  string  `json:"layout"`
	Package Package `json:"package"`
}

// stageDirectory keeps all incomplete output beside its destination, so the
// final rename stays on the same filesystem. Existing destinations are refused.
func stageDirectory(destination string) (string, error) {
	if destination == "" {
		return "", errors.New("destination is required")
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("destination already exists: %s", destination)
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0755); err != nil {
		return "", err
	}
	return os.MkdirTemp(parent, ".a8s-package-*")
}

func expose(stage, destination string) error {
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return err
		}
		return fmt.Errorf("destination already exists: %s", destination)
	}
	return os.Rename(stage, destination)
}

// Build creates an OCI image layout without executing source or requiring a
// runtime image. A source mutation during the build fails checksum comparison.
func Build(ctx context.Context, source, destination string) (BuildResult, error) {
	m, err := Inspect(ctx, source)
	if err != nil {
		return BuildResult{}, err
	}
	config, err := json.Marshal(m)
	if err != nil {
		return BuildResult{}, err
	}
	if int64(len(config)) > MaxMetadataBytes {
		return BuildResult{}, errors.New("package metadata exceeds size limit")
	}
	stage, err := stageDirectory(destination)
	if err != nil {
		return BuildResult{}, err
	}
	defer os.RemoveAll(stage)
	archive, err := os.CreateTemp(stage, "content-*.tar")
	if err != nil {
		return BuildResult{}, err
	}
	defer archive.Close()
	hash := sha256.New()
	tw := tar.NewWriter(io.MultiWriter(archive, hash))
	manifest, _ := json.Marshal(m.Manifest)
	if err := writeTarFile(tw, ManifestFile, 0444, manifest); err != nil {
		return BuildResult{}, err
	}
	root, err := os.OpenRoot(source)
	if err != nil {
		return BuildResult{}, err
	}
	defer root.Close()
	for _, expected := range m.Files {
		file, err := root.Open(expected.Path)
		if err != nil {
			return BuildResult{}, err
		}
		mode := int64(0444)
		if expected.Executable {
			mode = 0555
		}
		err = tw.WriteHeader(&tar.Header{Name: expected.Path, Size: expected.Size, Mode: mode, Typeflag: tar.TypeReg, Format: tar.FormatPAX})
		if err == nil {
			h := sha256.New()
			var n int64
			n, err = copyContent(ctx, io.MultiWriter(tw, h), file, expected.Size)
			if err == nil && (n != expected.Size || fmt.Sprintf("sha256:%x", h.Sum(nil)) != expected.Digest) {
				err = fmt.Errorf("source changed while building: %q", expected.Path)
			}
		}
		file.Close()
		if err != nil {
			return BuildResult{}, err
		}
	}
	if err := tw.Close(); err != nil {
		return BuildResult{}, err
	}
	size, err := archive.Seek(0, io.SeekCurrent)
	if err != nil {
		return BuildResult{}, err
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return BuildResult{}, err
	}
	layout := filepath.Join(stage, "layout")
	store, err := oci.NewWithContext(ctx, layout)
	if err != nil {
		return BuildResult{}, err
	}
	layer := ocispec.Descriptor{MediaType: LayerMediaType, Digest: digest.Digest(fmt.Sprintf("sha256:%x", hash.Sum(nil))), Size: size}
	if err := store.Push(ctx, layer, archive); err != nil {
		return BuildResult{}, err
	}
	configDesc, err := oras.PushBytes(ctx, store, ConfigMediaType, config)
	if err != nil {
		return BuildResult{}, err
	}
	desc, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_1, ArtifactType, oras.PackManifestOptions{
		ConfigDescriptor: &configDesc, Layers: []ocispec.Descriptor{layer},
		ManifestAnnotations: map[string]string{ocispec.AnnotationTitle: m.Manifest.Name, ocispec.AnnotationCreated: time.Unix(0, 0).UTC().Format(time.RFC3339)},
	})
	if err != nil {
		return BuildResult{}, err
	}
	if err := store.Tag(ctx, desc, layoutTag); err != nil {
		return BuildResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return BuildResult{}, err
	}
	if err := expose(layout, destination); err != nil {
		return BuildResult{}, err
	}
	return BuildResult{Layout: destination, Package: Package{Digest: desc.Digest.String(), Metadata: m}}, nil
}

func writeTarFile(tw *tar.Writer, name string, mode int64, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data)), Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

func openLayout(ctx context.Context, directory string) (*oci.Store, error) {
	if _, err := os.Stat(filepath.Join(directory, "oci-layout")); err != nil {
		return nil, err
	}
	return oci.NewWithContext(ctx, directory)
}

func fetchVerified(ctx context.Context, src content.Fetcher, desc ocispec.Descriptor, limit int64) ([]byte, error) {
	if err := validateDigest(desc.Digest.String()); err != nil {
		return nil, err
	}
	if desc.Size < 0 || desc.Size > limit {
		return nil, errors.New("OCI object exceeds size limit")
	}
	r, err := src.Fetch(ctx, desc)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, desc.Size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != desc.Size || digestBytes(data) != desc.Digest.String() {
		return nil, fmt.Errorf("OCI object checksum mismatch: %s", desc.Digest)
	}
	return data, ctx.Err()
}

func readPackage(ctx context.Context, src oras.ReadOnlyTarget, ref string) (Package, ocispec.Manifest, error) {
	desc, err := src.Resolve(ctx, ref)
	if err != nil {
		return Package{}, ocispec.Manifest{}, err
	}
	if strings.HasPrefix(ref, "sha256:") && desc.Digest.String() != ref {
		return Package{}, ocispec.Manifest{}, errors.New("resolved digest differs from requested digest")
	}
	if desc.MediaType != ocispec.MediaTypeImageManifest {
		return Package{}, ocispec.Manifest{}, fmt.Errorf("unsupported manifest media type %q", desc.MediaType)
	}
	data, err := fetchVerified(ctx, src, desc, MaxMetadataBytes)
	if err != nil {
		return Package{}, ocispec.Manifest{}, err
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Package{}, manifest, err
	}
	if manifest.SchemaVersion != 2 || manifest.ArtifactType != ArtifactType || manifest.Config.MediaType != ConfigMediaType || len(manifest.Layers) != 1 || manifest.Layers[0].MediaType != LayerMediaType {
		return Package{}, manifest, errors.New("unsupported a8s package OCI manifest")
	}
	layer := manifest.Layers[0]
	if err := validateDigest(layer.Digest.String()); err != nil {
		return Package{}, manifest, err
	}
	if layer.Size < 0 || layer.Size > maxArchiveBytes {
		return Package{}, manifest, errors.New("package archive exceeds size limit")
	}
	data, err = fetchVerified(ctx, src, manifest.Config, MaxMetadataBytes)
	if err != nil {
		return Package{}, manifest, err
	}
	var metadata Metadata
	if err := decodeJSON(data, &metadata); err != nil {
		return Package{}, manifest, err
	}
	if err := validateMetadata(metadata); err != nil {
		return Package{}, manifest, err
	}
	return Package{Digest: desc.Digest.String(), Metadata: metadata}, manifest, nil
}

// InspectLayout reads verified metadata from an existing local OCI layout.
// An empty reference selects the build's package tag.
func InspectLayout(ctx context.Context, directory, reference string) (Package, error) {
	store, err := openLayout(ctx, directory)
	if err != nil {
		return Package{}, err
	}
	if reference == "" {
		reference = layoutTag
	}
	p, _, err := readPackage(ctx, store, reference)
	return p, err
}

// MaterializeLayout requires a fixed digest, just like remote Materialize.
func MaterializeLayout(ctx context.Context, directory, fixedDigest, destination string) (Package, error) {
	if err := validateDigest(fixedDigest); err != nil {
		return Package{}, err
	}
	store, err := openLayout(ctx, directory)
	if err != nil {
		return Package{}, err
	}
	p, manifest, err := readPackage(ctx, store, fixedDigest)
	if err != nil {
		return Package{}, err
	}
	return p, materialize(ctx, store, p.Metadata, manifest.Layers[0], destination)
}

func materialize(ctx context.Context, src content.Fetcher, metadata Metadata, layer ocispec.Descriptor, destination string) error {
	stage, err := stageDirectory(destination)
	if err != nil {
		return err
	}
	defer removeStage(stage)
	// Verify the complete layer before parsing or exposing any content.
	archive, err := os.CreateTemp(filepath.Dir(stage), ".a8s-download-*.tar")
	if err != nil {
		return err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	r, err := src.Fetch(ctx, layer)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := copyContent(ctx, io.MultiWriter(archive, h), r, layer.Size)
	r.Close()
	if err != nil {
		return err
	}
	if n != layer.Size || fmt.Sprintf("sha256:%x", h.Sum(nil)) != layer.Digest.String() {
		return errors.New("package archive checksum mismatch")
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	root, err := os.OpenRoot(stage)
	if err != nil {
		return err
	}
	defer root.Close()
	expected := map[string]File{}
	for _, f := range metadata.Files {
		expected[f.Path] = f
	}
	seen := map[string]bool{}
	canonicalManifest, _ := json.Marshal(metadata.Manifest)
	tr := tar.NewReader(archive)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := packagePath(header.Name); err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg || header.Linkname != "" || seen[header.Name] {
			return fmt.Errorf("invalid or duplicate archive entry %q", header.Name)
		}
		seen[header.Name] = true
		if header.Name == ManifestFile {
			if header.Size != int64(len(canonicalManifest)) {
				return errors.New("archive manifest differs from OCI config")
			}
			data, err := io.ReadAll(io.LimitReader(tr, MaxMetadataBytes+1))
			if err != nil {
				return err
			}
			if !bytes.Equal(data, canonicalManifest) {
				return errors.New("archive manifest differs from OCI config")
			}
			if err := root.WriteFile(ManifestFile, data, 0444); err != nil {
				return err
			}
			continue
		}
		f, ok := expected[header.Name]
		if !ok || f.Size != header.Size {
			return fmt.Errorf("archive file is undeclared or has wrong size: %q", header.Name)
		}
		if err := root.MkdirAll(filepath.Dir(header.Name), 0755); err != nil {
			return err
		}
		file, err := root.OpenFile(header.Name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		h := sha256.New()
		n, copyErr := copyContent(ctx, io.MultiWriter(file, h), tr, f.Size)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if n != f.Size || fmt.Sprintf("sha256:%x", h.Sum(nil)) != f.Digest {
			return fmt.Errorf("file checksum mismatch: %q", f.Path)
		}
		mode := os.FileMode(0444)
		if f.Executable {
			mode = 0555
		}
		if err := root.Chmod(header.Name, mode); err != nil {
			return err
		}
	}
	if len(seen) != len(expected)+1 || !seen[ManifestFile] {
		return errors.New("archive is missing declared files")
	}
	// No unverified files or paths have been accepted. Freeze directories only
	// after extraction; Agentc writes derived configuration elsewhere.
	var directories []string
	err = filepath.WalkDir(stage, func(p string, entry os.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			directories = append(directories, p)
		}
		return err
	})
	if err != nil {
		return err
	}
	slices.Reverse(directories)
	for _, p := range directories {
		if err := os.Chmod(p, 0555); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return expose(stage, destination)
}

func removeStage(stage string) {
	// Failed exposure may leave read-only directories in the private staging tree.
	_ = filepath.WalkDir(stage, func(p string, entry os.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			_ = os.Chmod(p, 0700)
		}
		return nil
	})
	_ = os.RemoveAll(stage)
}
