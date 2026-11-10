package api

import (
	"errors"
	"fmt"

	v1 "github.com/jingyugao/a8s/api/gen/a8s/v1"
)

// Error preserves a safe, public classification separately from the private cause.
// Dependency adapters must supply safe messages/codes; raw response bodies must
// never be put in Detail. The cause is deliberately excluded from Error().
type Error struct {
	Detail *v1.ErrorDetail
	Cause  error
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Detail.Category, e.Detail.Message) }
func (e *Error) Unwrap() error { return e.Cause }

func NewError(category v1.ErrorCategory, message string) *Error {
	return &Error{Detail: &v1.ErrorDetail{Category: category, Message: message}}
}

func Detail(err error) *v1.ErrorDetail {
	var classified *Error
	if errors.As(err, &classified) {
		return classified.Detail
	}
	return &v1.ErrorDetail{Category: v1.ErrorCategory_ERROR_CATEGORY_DEPENDENCY_UNAVAILABLE, Message: "dependency unavailable; inspect server diagnostics"}
}

func IsCategory(err error, category v1.ErrorCategory) bool {
	var classified *Error
	return errors.As(err, &classified) && classified.Detail.Category == category
}
