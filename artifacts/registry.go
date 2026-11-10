package artifacts

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
)

// Credential is resolved by the application or CI, never by the package format.
type Credential struct {
	Username     string
	Password     string
	AccessToken  string
	RefreshToken string
}

type RegistryOptions struct {
	HTTPClient *http.Client
	Credential func(context.Context, string) (Credential, error)
	// PlainHTTP must be explicitly enabled for an HTTP registry.
	PlainHTTP bool
	// Timeout bounds a complete registry operation, including its content stream.
	// Zero selects two minutes. A caller deadline can shorten it.
	Timeout time.Duration
}

type Registry struct{ options RegistryOptions }

func NewRegistry(options RegistryOptions) (*Registry, error) {
	if options.Timeout < 0 {
		return nil, errors.New("registry timeout must not be negative")
	}
	if options.Timeout == 0 {
		options.Timeout = 2 * time.Minute
	}
	if options.HTTPClient == nil {
		options.HTTPClient = &http.Client{Timeout: options.Timeout}
	}
	return &Registry{options: options}, nil
}

func (r *Registry) repository(reference string) (*remote.Repository, registry.Reference, error) {
	// Reject URL userinfo and query parameters before any library error can
	// include them. OCI references use host/repository[:tag|@digest].
	if strings.Contains(reference, "://") || strings.ContainsAny(reference, "?#\r\n") {
		return nil, registry.Reference{}, errors.New("expected an OCI reference without URL credentials or query parameters")
	}
	ref, err := registry.ParseReference(reference)
	if err != nil {
		return nil, ref, errors.New("invalid OCI reference; expected host/repository:tag or host/repository@sha256:digest")
	}
	if ref.Reference == "" {
		return nil, ref, errors.New("explicit OCI tag or digest is required")
	}
	if strings.Contains(reference, "@") {
		if err := validateDigest(ref.Reference); err != nil {
			return nil, ref, err
		}
	}
	repo, err := remote.NewRepository(ref.Registry + "/" + ref.Repository)
	if err != nil {
		return nil, ref, err
	}
	repo.PlainHTTP = r.options.PlainHTTP
	client := &auth.Client{Client: r.options.HTTPClient, Cache: auth.NewCache()}
	if r.options.Credential != nil {
		client.Credential = func(ctx context.Context, host string) (auth.Credential, error) {
			credential, err := r.options.Credential(ctx, host)
			return auth.Credential{Username: credential.Username, Password: credential.Password, AccessToken: credential.AccessToken, RefreshToken: credential.RefreshToken}, err
		}
	}
	// auth.DefaultClient has automatic transport retries. This explicit client
	// performs auth negotiation without adding a generic retry layer.
	repo.Client = client
	return repo, ref, nil
}

type FixedReference struct {
	Reference string `json:"reference"`
	Digest    string `json:"digest"`
	MediaType string `json:"mediaType"`
}

// ResolveArtifact pins an arbitrary OCI artifact or image. Package consumers
// use Resolve, which additionally verifies the package config and requirements.
func (r *Registry) ResolveArtifact(ctx context.Context, reference string) (FixedReference, error) {
	ctx, cancel := context.WithTimeout(ctx, r.options.Timeout)
	defer cancel()
	repo, ref, err := r.repository(reference)
	if err != nil {
		return FixedReference{}, err
	}
	desc, err := repo.Resolve(ctx, ref.Reference)
	if err != nil {
		return FixedReference{}, err
	}
	if err := validateDigest(desc.Digest.String()); err != nil {
		return FixedReference{}, err
	}
	if strings.Contains(reference, "@") && ref.Reference != desc.Digest.String() {
		return FixedReference{}, errors.New("resolved digest differs from requested digest")
	}
	return FixedReference{Reference: ref.Registry + "/" + ref.Repository + "@" + desc.Digest.String(), Digest: desc.Digest.String(), MediaType: desc.MediaType}, nil
}

func (r *Registry) Resolve(ctx context.Context, reference string) (Package, error) {
	ctx, cancel := context.WithTimeout(ctx, r.options.Timeout)
	defer cancel()
	repo, ref, err := r.repository(reference)
	if err != nil {
		return Package{}, err
	}
	p, _, err := readPackage(ctx, repo, ref.Reference)
	if err != nil {
		return Package{}, err
	}
	p.Reference = ref.Registry + "/" + ref.Repository + "@" + p.Digest
	return p, nil
}

// Publish returns the content digest even if the network result is uncertain.
// On error, the caller can Resolve the returned fixed Reference to reconcile
// the upload; it must not infer that the registry rejected it.
func (r *Registry) Publish(ctx context.Context, layout, reference string) (Package, error) {
	ctx, cancel := context.WithTimeout(ctx, r.options.Timeout)
	defer cancel()
	repo, ref, err := r.repository(reference)
	if err != nil {
		return Package{}, err
	}
	store, err := openLayout(ctx, layout)
	if err != nil {
		return Package{}, err
	}
	p, _, err := readPackage(ctx, store, layoutTag)
	if err != nil {
		return Package{}, err
	}
	p.Reference = ref.Registry + "/" + ref.Repository + "@" + p.Digest
	if strings.Contains(reference, "@") && ref.Reference != p.Digest {
		return p, errors.New("publish destination digest does not match the local package")
	}
	_, err = oras.Copy(ctx, store, p.Digest, repo, ref.Reference, oras.DefaultCopyOptions)
	if err != nil {
		return p, fmt.Errorf("publish package %s: %w", p.Digest, err)
	}
	return p, nil
}

// Materialize only accepts a fixed reference. A complete verified package is
// atomically exposed in a new directory with read-only files and directories.
func (r *Registry) Materialize(ctx context.Context, fixedReference, destination string) (Package, error) {
	ctx, cancel := context.WithTimeout(ctx, r.options.Timeout)
	defer cancel()
	repo, ref, err := r.repository(fixedReference)
	if err != nil {
		return Package{}, err
	}
	if !strings.Contains(fixedReference, "@") {
		return Package{}, errors.New("materialize requires an immutable @sha256 reference; resolve the tag first")
	}
	p, manifest, err := readPackage(ctx, repo, ref.Reference)
	if err != nil {
		return Package{}, err
	}
	p.Reference = ref.Registry + "/" + ref.Repository + "@" + p.Digest
	return p, materialize(ctx, repo, p.Metadata, manifest.Layers[0], destination)
}

func (r *Registry) Fetch(ctx context.Context, fixedReference, destination string) (Package, error) {
	return r.Materialize(ctx, fixedReference, destination)
}
