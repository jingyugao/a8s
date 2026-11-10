package releases

import (
	"encoding/hex"
	"strings"

	"github.com/jingyugao/a8s/api"
	v1 "github.com/jingyugao/a8s/api/gen/a8s/v1"
)

func ValidateArtifact(a *v1.Artifact) error {
	if strings.Contains(a.GetReference(), "://") || strings.ContainsAny(a.GetReference(), "?#\r\n") {
		return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_INVALID, "artifact reference must not contain URL credentials or query parameters")
	}
	if strings.Contains(a.GetReference(), "@") {
		_, pinned, _ := strings.Cut(a.Reference, "@")
		if pinned != a.Digest {
			return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_INVALID, "artifact reference digest must match its fixed digest")
		}
	}
	algorithm, encoded, ok := strings.Cut(a.GetDigest(), ":")
	length := 0
	switch algorithm {
	case "sha256":
		length = 64
	case "sha512":
		length = 128
	}
	_, err := hex.DecodeString(encoded)
	if !ok || length == 0 || len(encoded) != length || err != nil || encoded != strings.ToLower(encoded) || a.GetReference() == "" {
		return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_INVALID, "artifact requires a reference and a canonical sha256/sha512 digest")
	}
	return nil
}

func FixedReference(a *v1.Artifact) string {
	repository := strings.SplitN(a.Reference, "@", 2)[0]
	if colon := strings.LastIndex(repository, ":"); colon > strings.LastIndex(repository, "/") {
		repository = repository[:colon]
	}
	return repository + "@" + a.Digest
}

func CheckCapabilities(required, available []string) error {
	have := make(map[string]bool, len(available))
	for _, capability := range available {
		have[capability] = true
	}
	for _, capability := range required {
		if !have[capability] {
			return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_INCOMPATIBLE, "runtime does not provide a required package capability")
		}
	}
	return nil
}
