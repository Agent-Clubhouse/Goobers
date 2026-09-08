package engine

import (
	"errors"
	"time"

	"go.temporal.io/sdk/temporal"

	"github.com/goobers/goobers/internal/dispatcher"
)

// A failed activity's result is not transported by Temporal. Its observed
// placement therefore travels with the classified error. Details slot zero
// remains the established infrastructure retry-at instant; slot one is an
// explicitly versioned extension. Older consumers can still read slot zero.
type dispatchFailureEvidence struct {
	SchemaVersion int             `json:"schemaVersion"`
	Placement     *StagePlacement `json:"placement"`
}

func withDispatchFailurePlacement(classified error, report dispatcher.Report) error {
	if classified == nil || report.Local || report.Runner == "" {
		return classified
	}
	var appErr *temporal.ApplicationError
	if !errors.As(classified, &appErr) {
		return classified
	}
	var retryAt time.Time
	if appErr.HasDetails() && readDispatchFailureDetails(appErr, &retryAt) != nil {
		// An unfamiliar existing detail must not be replaced or interpreted
		// as placement. The original failure and retry behavior take priority.
		return classified
	}
	var extra interface{}
	if appErr.HasDetails() && readDispatchFailureDetails(appErr, &retryAt, &extra) == nil {
		return classified // Never replace another extension's detail slots.
	}
	return temporal.NewApplicationErrorWithOptions(appErr.Message(), appErr.Type(), temporal.ApplicationErrorOptions{
		NonRetryable:   appErr.NonRetryable(),
		Cause:          appErr.Unwrap(),
		NextRetryDelay: appErr.NextRetryDelay(),
		Category:       appErr.Category(),
		Details: []interface{}{retryAt, dispatchFailureEvidence{
			SchemaVersion: 1,
			Placement:     placementProvenance(report),
		}},
	})
}

func dispatchFailureResult(err error, report dispatcher.Report) (stageActivityResult, error) {
	return stageActivityResult{}, withDispatchFailurePlacement(err, report)
}

// DispatchFailurePlacement recovers substrate observations from a failed
// DispatchStage activity or DispatchOne workflow. Missing/older/unknown details
// yield nil. A pre-create refusal can name a selected runner without inventing
// a pod, node, image, or timestamp that was never observed.
func DispatchFailurePlacement(err error) *StagePlacement {
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || !appErr.HasDetails() {
		return nil
	}
	if appErr.Type() != FailureTypeInfrastructure && appErr.Type() != FailureTypeStage {
		return nil
	}
	var retryAt time.Time
	var evidence dispatchFailureEvidence
	if readDispatchFailureDetails(appErr, &retryAt, &evidence) != nil || evidence.SchemaVersion != 1 || evidence.Placement == nil || evidence.Placement.Runner == "" {
		return nil
	}
	return evidence.Placement
}

// Encoded SDK details return a decoding error for an incompatible payload;
// freshly constructed SDK errors instead use a reflective getter that panics
// on the same type mismatch. Confine that SDK behavior to this decode boundary
// so unknown metadata has the same absent-evidence result in either form.
func readDispatchFailureDetails(err *temporal.ApplicationError, values ...interface{}) (decodeErr error) {
	defer func() {
		if recover() != nil {
			decodeErr = errors.New("incompatible dispatch failure details")
		}
	}()
	return err.Details(values...)
}
