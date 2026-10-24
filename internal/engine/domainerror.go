package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"

	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/telemetry"
)

// domainFailure preserves the domain error before Temporal serializes it. It
// extends evidence v1 additively: missing envelopes retain the N-1 journal
// behavior. This is presentation evidence, never a retry or routing decision.
type domainFailure struct {
	Error              *journal.ErrorDetail `json:"error"`
	Code               string               `json:"code"`
	InfraCommittedWork bool                 `json:"infraCommittedWork,omitempty"`
}

func withDomainFailure(classified, domain error, committed bool) error {
	if domain == nil {
		return classified
	}
	var app *temporal.ApplicationError
	if !errors.As(classified, &app) {
		return classified
	}
	var retryAt time.Time
	if app.HasDetails() && readDispatchFailureDetails(app, &retryAt) != nil {
		return classified
	}
	code, _ := runsemantics.ClassifyDispatchError(domain)
	evidence := dispatchFailureEvidence{SchemaVersion: dispatchFailureEvidenceVersion,
		Domain: &domainFailure{Error: journal.ErrorDetailFor("executor_error", domain), Code: code, InfraCommittedWork: committed}}
	return temporal.NewApplicationErrorWithOptions(app.Message(), app.Type(), temporal.ApplicationErrorOptions{
		NonRetryable: app.NonRetryable(), Cause: app.Unwrap(), NextRetryDelay: app.NextRetryDelay(), Category: app.Category(),
		Details: []interface{}{retryAt, evidence},
	})
}

func domainFailureFromError(err error) *domainFailure {
	for current := err; current != nil; current = errors.Unwrap(current) {
		var app *temporal.ApplicationError
		if !errors.As(current, &app) || !app.HasDetails() {
			continue
		}
		var retryAt time.Time
		var evidence dispatchFailureEvidence
		if readDispatchFailureDetails(app, &retryAt, &evidence) == nil && evidence.SchemaVersion == dispatchFailureEvidenceVersion && evidence.Domain != nil && evidence.Domain.Error != nil {
			return evidence.Domain
		}
	}
	return nil
}

func domainErrorDetailFor(code string, err error) *journal.ErrorDetail {
	if domain := domainFailureFromError(err); domain != nil {
		detail := *domain.Error
		detail.Code = code
		return &detail
	}
	return journal.ErrorDetailFor(code, err)
}

func dispatchJournalClassification(err error, failureClass journal.AttemptClass) (string, telemetry.ErrorClass) {
	if domain := domainFailureFromError(err); domain != nil {
		return domain.Code, telemetry.ClassifyError(domain.Code)
	}
	// Old activity histories have no domain envelope. Preserve their existing
	// class/code mapping; a transport wrapper cannot recover a typed domain code.
	code := telemetry.ErrCodeExecutor
	if failureClass == journal.AttemptInfra {
		code = telemetry.ErrCodeInfraFailure
	}
	return code, telemetry.ClassifyError(code)
}

func classifyAgenticFailure(ctx context.Context, ws Workspace, err error) error {
	var committed bool
	if inspector, ok := ws.(CommitInspector); ok && invoke.IsInfrastructureFailure(err) {
		var inspectErr error
		committed, inspectErr = inspector.HasNewCommits(ctx)
		if inspectErr != nil {
			committed = false
			err = errors.Join(err, fmt.Errorf("inspect commits created by infrastructure-failed attempt: %w", inspectErr))
		}
	}
	return withDomainFailure(classifySeamTransportError(err), err, committed)
}
