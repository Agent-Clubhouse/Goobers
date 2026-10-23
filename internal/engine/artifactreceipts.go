package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

const artifactReceiptsChange = "stage-recorder-artifact-receipts-v1"

func collectArtifactReceipts(ctx context.Context) (context.Context, *invoke.ArtifactReceipts) {
	receipts := &invoke.ArtifactReceipts{}
	return invoke.WithArtifactReporter(ctx, receipts.Record), receipts
}

func finishArtifactReceipts(receipts *invoke.ArtifactReceipts, result *stageActivityResult, err *error) {
	recorded := receipts.Snapshot()
	if len(recorded) == 0 {
		return
	}
	if *err == nil {
		result.RecordedArtifacts = recorded
		return
	}
	*err = withArtifactReceipts(*err, recorded)
}

// Preserve retry-at and the existing failure-evidence envelope. Unknown future
// evidence is not overwritten, and an N-1 failure with no receipts stays valid.
func withArtifactReceipts(err error, receipts []invoke.ArtifactReceipt) error {
	var app *temporal.ApplicationError
	if !errors.As(err, &app) {
		return err
	}
	retryAt, evidence, ok := artifactFailureEvidence(app)
	if !ok {
		return err
	}
	evidence.RecordedArtifacts = receipts
	return temporal.NewApplicationErrorWithOptions(app.Message(), app.Type(), temporal.ApplicationErrorOptions{
		NonRetryable: app.NonRetryable(), Cause: app.Unwrap(), NextRetryDelay: app.NextRetryDelay(), Category: app.Category(), Details: []any{retryAt, evidence},
	})
}

func artifactFailureEvidence(app *temporal.ApplicationError) (time.Time, dispatchFailureEvidence, bool) {
	var retryAt time.Time
	evidence := dispatchFailureEvidence{SchemaVersion: dispatchFailureEvidenceVersion}
	if !app.HasDetails() {
		return retryAt, evidence, true
	}
	if readDispatchFailureDetails(app, &retryAt) != nil {
		return retryAt, evidence, false
	}
	var extra any
	if readDispatchFailureDetails(app, &retryAt, &extra) != nil {
		return retryAt, evidence, true
	}
	// Older activity adapters attach a typed usage detail before returning.
	// Preserve that known in-process payload when extending the evidence;
	// never guess the type of an unknown serialized extension.
	if usage, ok := extra.(AttemptUsage); ok {
		evidence.Usage = &usage
		return retryAt, evidence, true
	}
	if readDispatchFailureDetails(app, &retryAt, &evidence) != nil || evidence.SchemaVersion != dispatchFailureEvidenceVersion {
		return retryAt, evidence, false
	}
	return retryAt, evidence, true
}

// Workerhost carries attempt identity in an outer ApplicationError; recorder
// evidence remains in its cause. Traverse wrappers without inventing receipts
// for N-1 or unrecognized evidence.
func failedArtifactReceipts(err error) []invoke.ArtifactReceipt {
	for current := err; current != nil; current = errors.Unwrap(current) {
		var app *temporal.ApplicationError
		if !errors.As(current, &app) {
			continue
		}
		_, evidence, ok := artifactFailureEvidence(app)
		if ok && len(evidence.RecordedArtifacts) > 0 {
			return evidence.RecordedArtifacts
		}
	}
	return nil
}

func (r *runJournal) recordArtifactReceipts(ctx workflow.Context, receipts []invoke.ArtifactReceipt, dispatchErr error) error {
	if workflow.GetVersion(ctx, artifactReceiptsChange, workflow.DefaultVersion, 1) == workflow.DefaultVersion {
		return nil
	}
	if dispatchErr != nil {
		receipts = failedArtifactReceipts(dispatchErr)
	}
	for _, receipt := range receipts {
		ref := journalRefFrom(receipt.Ref)
		canonical, err := journal.ArtifactPath(ref.Digest)
		if receipt.Name == "" || err != nil || canonical != ref.Path || ref.Size < 0 || !ref.Integrity.Valid() {
			return fmt.Errorf("engine: invalid recorded artifact receipt %q", receipt.Name)
		}
		// A bare recorder has no stage scope; the logical name/grade come from
		// that recorder exactly as on the local Run.RecordArtifact path.
		r.artifactAt(workflow.Now(ctx), JournalArtifactOp{Name: receipt.Name, Ref: &ref, Integrity: ref.Integrity})
	}
	return nil
}

// Record dispatch evidence in journal order before its outcome. Receipts must
// precede failure/finish events that can reference the recorded artifacts.
func (r *runJournal) recordDispatchEvidence(ctx workflow.Context, stage string, attempt int, class journal.AttemptClass, result stageActivityResult, dispatchErr error) error {
	r.placement(ctx, stage, attempt, class, result)
	if err := r.recordArtifactReceipts(ctx, result.RecordedArtifacts, dispatchErr); err != nil {
		return err
	}
	r.recordDeferredRunBranch(ctx, dispatchErr, result.ResultEnvelope, len(result.Mutations) > 0)
	return nil
}
