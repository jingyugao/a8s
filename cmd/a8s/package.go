package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/jingyugao/a8s/artifacts"
)

func packageCommand(ctx context.Context, command string, args []string, c *config, stdout, stderr io.Writer) (any, error) {
	fs := newFlags("a8s package "+command, c, stderr)
	source := fs.String("source", ".", "source directory with a8s-package.json")
	output := fs.String("output", "", "new output directory")
	layout := fs.String("layout", "", "existing OCI image layout")
	reference := fs.String("ref", "", "OCI host/repository:tag or @sha256:digest")
	fixedDigest := fs.String("digest", "", "fixed local layout digest")
	name := fs.String("name", "", "package name (init)")
	instructions := fs.String("instructions", "AGENTS.md", "instructions path (init)")
	plainHTTP := fs.Bool("plain-http", false, "use HTTP for an explicitly selected registry")
	artifact := fs.Bool("artifact", false, "resolve an Agentc or sandbox image instead of a package")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() != 0 {
		return nil, errors.New("unexpected positional arguments; use named flags")
	}
	if c.timeout <= 0 {
		return nil, errors.New("timeout must be positive")
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	switch command {
	case "init":
		return initializePackage(ctx, *source, *name, *instructions)
	case "validate":
		if err := artifacts.Validate(ctx, *source); err != nil {
			return nil, err
		}
		return map[string]any{"valid": true, "source": *source}, nil
	case "inspect":
		if *layout != "" {
			return artifacts.InspectLayout(ctx, *layout, *fixedDigest)
		}
		if *reference != "" {
			registry, err := registryClient(c, *plainHTTP, *reference)
			if err != nil {
				return nil, err
			}
			return registry.Resolve(ctx, *reference)
		}
		return artifacts.Inspect(ctx, *source)
	case "build":
		if *output == "" {
			return nil, errors.New("--output is required")
		}
		return artifacts.Build(ctx, *source, *output)
	case "publish":
		if *layout == "" || *reference == "" {
			return nil, errors.New("--layout and --ref are required")
		}
		registry, err := registryClient(c, *plainHTTP, *reference)
		if err != nil {
			return nil, err
		}
		pkg, err := registry.Publish(ctx, *layout, *reference)
		if pkg.Digest == "" {
			return nil, err
		}
		return pkg, err
	case "resolve":
		if *reference == "" {
			return nil, errors.New("--ref is required")
		}
		registry, err := registryClient(c, *plainHTTP, *reference)
		if err != nil {
			return nil, err
		}
		if *artifact {
			return registry.ResolveArtifact(ctx, *reference)
		}
		return registry.Resolve(ctx, *reference)
	case "fetch":
		if *output == "" {
			return nil, errors.New("--output is required")
		}
		if *layout != "" {
			return artifacts.MaterializeLayout(ctx, *layout, *fixedDigest, *output)
		}
		if *reference == "" {
			return nil, errors.New("--ref or --layout with --digest is required")
		}
		registry, err := registryClient(c, *plainHTTP, *reference)
		if err != nil {
			return nil, err
		}
		return registry.Materialize(ctx, *reference, *output)
	default:
		return nil, fmt.Errorf("unknown package command %q", command)
	}
}

func registryClient(c *config, plainHTTP bool, reference string) (*artifacts.Registry, error) {
	username, password, token := os.Getenv("A8S_REGISTRY_USERNAME"), os.Getenv("A8S_REGISTRY_PASSWORD"), os.Getenv("A8S_REGISTRY_TOKEN")
	host, _, _ := strings.Cut(reference, "/")
	return artifacts.NewRegistry(artifacts.RegistryOptions{HTTPClient: &http.Client{Timeout: c.timeout}, PlainHTTP: plainHTTP, Timeout: c.timeout, Credential: func(_ context.Context, target string) (artifacts.Credential, error) {
		if target != host {
			return artifacts.Credential{}, nil
		}
		return artifacts.Credential{Username: username, Password: password, AccessToken: token}, nil
	}})
}

func initializePackage(ctx context.Context, directory, name, instructions string) (any, error) {
	if name == "" {
		return nil, errors.New("--name is required")
	}
	if filepath.IsAbs(instructions) || filepath.ToSlash(filepath.Clean(instructions)) != instructions || instructions == "." || strings.HasPrefix(instructions, "../") {
		return nil, errors.New("instructions must be a relative package path")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	info, err := root.Lstat(instructions)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("instructions must be a regular file")
	}
	m := artifacts.Manifest{SchemaVersion: artifacts.FormatVersion, Name: name, Instructions: instructions, Contents: []string{instructions}}
	entries, err := fs.ReadDir(root.FS(), ".agents/skills")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			return nil, fmt.Errorf("unexpected skill entry %q; index it explicitly", entry.Name())
		}
		path := ".agents/skills/" + entry.Name()
		m.Skills = append(m.Skills, artifacts.Skill{Name: entry.Name(), Path: path})
		m.Contents = append(m.Contents, path)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	file, err := root.OpenFile(artifacts.ManifestFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return nil, err
	}
	_, err = file.Write(append(data, '\n'))
	closeErr := file.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if err := artifacts.Validate(ctx, directory); err != nil {
		_ = root.Remove(artifacts.ManifestFile)
		return nil, err
	}
	return m, nil
}
