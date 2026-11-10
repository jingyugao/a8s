package api

import (
	v1 "github.com/jingyugao/a8s/api/gen/a8s/v1"
	"strings"
)

// ScopeKey is the canonical opaque scope used by Agentc and Provider contracts.
// Resource scopes consist of validated IDs, neither of which contains '/'.
func ScopeKey(scope *v1.Scope) string { return scope.GetTenantId() + "/" + scope.GetProjectId() }

func ParseScope(key string) (*v1.Scope, error) {
	tenant, project, ok := strings.Cut(key, "/")
	if !ok || tenant == "" || project == "" || strings.Contains(project, "/") {
		return nil, NewError(v1.ErrorCategory_ERROR_CATEGORY_INVALID, "scope must be tenant_id/project_id")
	}
	return &v1.Scope{TenantId: tenant, ProjectId: project}, nil
}
