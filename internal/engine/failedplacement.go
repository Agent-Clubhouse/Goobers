package engine

import (
	"errors"
	"maps"
	"time"

	"go.temporal.io/sdk/temporal"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/invoke"
)

// A failed activity's result is not transported by Temporal. Its observed
// placement and trusted usage evidence therefore travel with the classified
// error. Details slot zero remains the established infrastructure retry-at
// instant; slot one is an explicitly versioned extension. Older consumers can
// still read slot zero.
type dispatchFailureEvidence struct {
	Domain            *domainFailure           `json:"domain,omitempty"`
	RecordedArtifacts []invoke.ArtifactReceipt `json:"recordedArtifacts,omitempty"`
	SchemaVersion     int                      `json:"schemaVersion"`
	Placement         *StagePlacement          `json:"placement,omitempty"`
	Usage             *AttemptUsage            `json:"usage,omitempty"`
	UsageUnavailable  bool                     `json:"usageUnavailable,omitempty"`
}

func withDispatchFailurePlacement(classified error, report dispatcher.Report) error {
	return withDispatchFailureEvidence(classified, report, nil, false)
}

const dispatchFailureEvidenceVersion = 1

func withDispatchFailureEvidence(classified error, report dispatcher.Report, usage *AttemptUsage, usageUnavailable bool) error {
	if classified == nil {
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
	evidence := dispatchFailureEvidence{SchemaVersion: dispatchFailureEvidenceVersion}
	if appErr.HasDetails() {
		var extra interface{}
		if readDispatchFailureDetails(appErr, &retryAt, &extra) == nil {
			if readDispatchFailureDetails(appErr, &retryAt, &evidence) != nil ||
				evidence.SchemaVersion != dispatchFailureEvidenceVersion {
				return classified // Never replace another extension's detail slots.
			}
		}
	}
	if !report.Local && report.Runner != "" {
		evidence.Placement = placementProvenance(report)
	}
	if usage != nil {
		copy := *usage
		copy.Metrics = maps.Clone(usage.Metrics)
		copy.Unavailable = copy.Unavailable || usageUnavailable
		evidence.Usage = &copy
	}
	evidence.UsageUnavailable = evidence.UsageUnavailable || usageUnavailable
	if evidence.Placement == nil && evidence.Usage == nil && !evidence.UsageUnavailable {
		return classified
	}
	return temporal.NewApplicationErrorWithOptions(appErr.Message(), appErr.Type(), temporal.ApplicationErrorOptions{
		NonRetryable:   appErr.NonRetryable(),
		Cause:          appErr.Unwrap(),
		NextRetryDelay: appErr.NextRetryDelay(),
		Category:       appErr.Category(),
		Details:        []interface{}{retryAt, evidence},
	})
}

func dispatchFailureResult(err error, report dispatcher.Report) (stageActivityResult, error) {
	return stageActivityResult{}, withDispatchFailurePlacement(err, report)
}

func dispatchFailureResultWithUsage(err error, report dispatcher.Report, usage *AttemptUsage, unavailable bool) (stageActivityResult, error) {
	return stageActivityResult{}, withDispatchFailureEvidence(err, report, usage, unavailable)
}

// DispatchFailurePlacement recovers substrate observations from a failed
// DispatchStage activity or DispatchOne workflow. Missing/older/unknown details
// yield nil. A pre-create refusal can name a selected runner without inventing
// a pod, node, image, or timestamp that was never observed.
func DispatchFailurePlacement(err error) *StagePlacement {
	for current := err; current != nil; current = errors.Unwrap(current) {
		var appErr *temporal.ApplicationError
		if !errors.As(current, &appErr) || !appErr.HasDetails() {
			continue
		}
		if appErr.Type() != FailureTypeInfrastructure && appErr.Type() != FailureTypeStage {
			continue
		}
		var retryAt time.Time
		var evidence dispatchFailureEvidence
		if readDispatchFailureDetails(appErr, &retryAt, &evidence) == nil &&
			evidence.SchemaVersion == dispatchFailureEvidenceVersion &&
			evidence.Placement != nil && evidence.Placement.Runner != "" {
			return evidence.Placement
		}
	}
	return nil
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
