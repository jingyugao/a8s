package kubernetes

import (
	"errors"

	provider "github.com/jingyugao/a8s/providers/contract"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func fault(category provider.ErrorCategory, code, message string) *provider.Error {
	return &provider.Error{Category: category, Source: "kubernetes", Code: code, Message: message}
}

func wrap(err error, mutation bool, operationID, resourceID string) *provider.Error {
	var known *provider.Error
	if errors.As(err, &known) {
		result := *known
		return &result
	}
	result := fault(provider.DependencyUnavailable, "DependencyUnavailable", "Kubernetes request could not be confirmed; inspect the original resource")
	result.Cause, result.OperationID, result.ResourceID = err, operationID, resourceID
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		detail := status.Status()
		result.Code, result.HTTPStatus = string(detail.Reason), int(detail.Code)
		// Status messages can echo container arguments/environment. Preserve the
		// exact reason/status and unwrap-able cause, not the raw remote body.
	}
	switch {
	case apierrors.IsNotFound(err):
		result.Category = provider.NotFound
	case apierrors.IsConflict(err), apierrors.IsAlreadyExists(err):
		result.Category = provider.Conflict
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		result.Category = provider.Forbidden
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		result.Category = provider.Invalid
	case mutation:
		result.Category = provider.OutcomeUnknown
	}
	return result
}

func unknown(err error, operationID, resourceID string) *provider.Error {
	result := wrap(err, true, operationID, resourceID)
	result.Category = provider.OutcomeUnknown
	return result
}
