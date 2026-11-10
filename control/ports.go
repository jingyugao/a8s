package control

import (
	"context"

	v1 "github.com/jingyugao/a8s/api/gen/a8s/v1"
	"github.com/jingyugao/a8s/control/resources"
)

type PackageResolution struct {
	Artifact             *v1.Artifact
	RequiredCapabilities []string
}

// Resolver is implemented by the application using Artifacts' public API.
// ValidateRelease checks fixed artifact availability, credential references and
// native-state compatibility, including rollback, without persisting secrets.
type Resolver interface {
	ResolvePackage(context.Context, string) (*PackageResolution, error)
	ValidateRelease(context.Context, *v1.Release, *v1.Release) error
}

type Action struct {
	Scope        *v1.Scope
	DeploymentID string
	Workspace    *v1.Workspace
	Release      *v1.Release
	Instance     *v1.Instance
	RequestKey   string
	ExternalID   string
	Step         string
}

// Environment adapters belong in assembly and use providers/contract, never
// Cellbox internals. Lookup must correlate the original key even when the
// response carrying the external operation ID was lost. Idempotent is an
// explicit guarantee, not inferred from support for a caller-supplied label.
type Environment interface {
	SupportsWriterRelease() bool
	// Plan performs reads only and returns a durable reconstruction of the
	// original provider request, including its reserved compute identity.
	Plan(context.Context, Action) (string, error)
	Idempotent(step string) bool
	Lookup(context.Context, Action) (*resources.Observation, error)
	Prepare(context.Context, Action) (*resources.Observation, error)
	StopCompute(context.Context, Action) (*resources.Observation, error)
	ReleaseCompute(context.Context, Action) (*resources.Observation, error)
}

type Executor interface {
	Idempotent(step string) bool
	Lookup(context.Context, Action) (*resources.Observation, error)
	Bind(context.Context, Action) (*resources.Observation, error)
	OpenAdmission(context.Context, Action) (*resources.Observation, error)
	CloseAdmission(context.Context, Action) (*resources.Observation, error)
	Drain(context.Context, Action) (*resources.Observation, error)
	Stop(context.Context, Action) (*resources.Observation, error)
}

type Dependencies struct {
	Resolver    Resolver
	Environment Environment
	Executor    Executor
}
