package main

import (
	"context"

	"github.com/jingyugao/a8s/api"
	v1 "github.com/jingyugao/a8s/api/gen/a8s/v1"
	"github.com/jingyugao/a8s/artifacts"
	"github.com/jingyugao/a8s/control"
	"github.com/jingyugao/a8s/control/releases"
)

type artifactAdapter struct{ registry *artifacts.Registry }

func (a *artifactAdapter) ResolvePackage(ctx context.Context, reference string) (*control.PackageResolution, error) {
	p, err := a.registry.Resolve(ctx, reference)
	if err != nil {
		return nil, registryError(err)
	}
	return &control.PackageResolution{Artifact: &v1.Artifact{Reference: p.Reference, Digest: p.Digest}, RequiredCapabilities: p.Metadata.Manifest.Requirements.Capabilities}, nil
}

func registryError(cause error) error {
	err := api.NewError(v1.ErrorCategory_ERROR_CATEGORY_DEPENDENCY_UNAVAILABLE, "artifact registry could not verify the fixed reference")
	err.Detail.Source = "artifacts"
	err.Cause = cause
	return err
}

func (a *artifactAdapter) ValidateRelease(ctx context.Context, release, previous *v1.Release) error {
	if release.Agentc.HostDigest == "" {
		return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_INCOMPATIBLE, "Codex requires a fixed host_digest for executable bytes")
	}
	if release.Agentc.ProtocolVersion != "v1" && release.Agentc.ProtocolVersion != "a8s.agentc.v1" {
		return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_UNSUPPORTED, "server only supports the a8s.agentc.v1 protocol")
	}
	p, err := a.registry.Resolve(ctx, releases.FixedReference(release.Package))
	if err != nil {
		return registryError(err)
	}
	if p.Digest != release.Package.Digest {
		return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_INCOMPATIBLE, "fixed package digest changed")
	}
	requirements := p.Metadata.Manifest.Requirements
	if requirements.AgentcProtocol != "" && requirements.AgentcProtocol != release.Agentc.ProtocolVersion {
		return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_INCOMPATIBLE, "package requires another Agentc protocol version")
	}
	if len(requirements.Platforms) > 0 {
		compatible := false
		for _, platform := range requirements.Platforms {
			if platform.OS == "linux" && platform.Architecture == release.Runtime.Sandbox.Architecture {
				compatible = true
			}
		}
		if !compatible {
			return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_INCOMPATIBLE, "sandbox platform does not match package requirements")
		}
	}
	if len(requirements.Runtimes) > 0 || len(requirements.System) > 0 {
		return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_UNSUPPORTED, "sandbox dependency declarations are not yet verified by this server")
	}
	if len(release.Spec.Credentials) > 0 {
		return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_UNSUPPORTED, "credential reference validation requires a host-injected resolver")
	}
	if previous != nil && (previous.Agentc.NativeStateVersion == "" || release.Agentc.NativeStateVersion == "" || previous.Agentc.NativeStateVersion != release.Agentc.NativeStateVersion) {
		return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_INCOMPATIBLE, "native history schema compatibility has not been verified")
	}
	for _, artifact := range []*v1.Artifact{release.Agentc.Artifact, release.Runtime.Sandbox.Image} {
		fixed, err := a.registry.ResolveArtifact(ctx, releases.FixedReference(artifact))
		if err != nil {
			return registryError(err)
		}
		if fixed.Digest != artifact.Digest {
			return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_INCOMPATIBLE, "fixed runtime artifact digest changed")
		}
	}
	return nil
}
