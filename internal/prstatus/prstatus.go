// Package prstatus publishes pull-request status evidence and persists the
// result consumed by later workflow stages. Command adapters remain responsible
// for provider construction, input defaults, integrity finalization, and
// operator-facing error mapping.
package prstatus

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/goobers/goobers/providers"
)

// ResultWriteError reports a failure after the provider accepted the status.
// Callers can distinguish it from an unchanged provider error without
// republishing the status.
type ResultWriteError struct {
	cause error
}

func (e *ResultWriteError) Error() string {
	return e.cause.Error()
}

func (e *ResultWriteError) Unwrap() error {
	return e.cause
}

// ParseState maps workflow status aliases to the provider-neutral check state.
func ParseState(value string) (providers.CheckState, error) {
	switch value {
	case "succeeded", "success", "passing":
		return providers.CheckStatePassing, nil
	case "failed", "failure", "failing":
		return providers.CheckStateFailing, nil
	case "pending", "":
		return providers.CheckStatePending, nil
	default:
		return "", fmt.Errorf("unknown status state %q (want succeeded|failed|pending)", value)
	}
}

// Publish forwards request exactly once and persists its provider result.
func Publish(
	ctx context.Context,
	publisher providers.PullRequestStatusPublisher,
	request providers.PullRequestStatusRequest,
	resultFile string,
) (providers.PullRequestStatusResult, error) {
	result, err := publisher.PublishPullRequestStatus(ctx, request)
	if err != nil {
		return providers.PullRequestStatusResult{}, err
	}

	output := map[string]string{
		"statusId":    strconv.Itoa(result.ID),
		"statusGenre": request.Genre,
		"statusName":  request.Name,
		"state":       string(request.State),
		"prNumber":    request.PullID,
	}
	data, err := json.Marshal(output)
	if err != nil {
		return result, &ResultWriteError{cause: fmt.Errorf("marshal status result: %w", err)}
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		return result, &ResultWriteError{cause: fmt.Errorf("write %s: %w", resultFile, err)}
	}
	return result, nil
}
