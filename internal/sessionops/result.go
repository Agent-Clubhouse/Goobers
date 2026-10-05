package sessionops

import (
	"errors"
	"net/http"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/workbench"
)

func validateResult(value any, gaggle, binding string) error {
	validate := func(item workbench.BacklogItem) error {
		scope := workbench.Scope{GaggleID: gaggle, Bindings: map[string]bool{binding: true}}
		if err := scope.ValidateRef(item.Ref); err != nil {
			return ErrDenied
		}
		if item.Ref.Kind != "work-item" || item.Locator.ID == "" {
			return ErrDenied
		}
		return nil
	}
	switch result := value.(type) {
	case workbench.BacklogItem:
		return validate(result)
	case workbench.BacklogPage:
		if len(result.Items) > workbench.MaxBacklogPageItems || len(result.NextCursor) > workbench.MaxBacklogCursorBytes {
			return ErrLimit
		}
		for _, item := range result.Items {
			if err := validate(item); err != nil {
				return err
			}
		}
		return nil
	default:
		return ErrDenied
	}
}
func operationError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrDenied) {
		return httpapi.NewInterventionError(http.StatusForbidden, "session_operation_denied", "The session operation is no longer authorized.", nil)
	}
	if errors.Is(err, ErrLimit) {
		return httpapi.NewInterventionError(http.StatusTooManyRequests, "session_operation_limit", "This turn reached its source-operation allowance.", nil)
	}
	return httpapi.NewInterventionError(http.StatusServiceUnavailable, "session_operation_unavailable", "The source operation could not be completed.", nil)
}
