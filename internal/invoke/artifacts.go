package invoke

import (
	"context"
	"sync"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// ArtifactReceipt is a recorder's observation, not a stage-authored output.
// Name is the logical name passed to RecordArtifact, never inferred from Path.
// Ref addresses bytes that the recorder has already scrubbed and stored.
type ArtifactReceipt struct {
	Name string                `json:"name"`
	Ref  apiv1.ArtifactPointer `json:"ref"`
}

type artifactReporterKey struct{}

// WithArtifactReporter attaches the activity's trusted recorder observer.
func WithArtifactReporter(ctx context.Context, report func(ArtifactReceipt)) context.Context {
	return context.WithValue(ctx, artifactReporterKey{}, report)
}

// ArtifactReporter binds a per-invocation recorder to its activity. A nil
// observer preserves local execution and older workers' existing behavior.
func ArtifactReporter(ctx context.Context) func(ArtifactReceipt) {
	report, _ := ctx.Value(artifactReporterKey{}).(func(ArtifactReceipt))
	return report
}

// ArtifactReceipts preserves recorder order, including repeated writes of the
// same content/name. Those are separate journal events in local execution too.
// It is scoped to one invocation and safe for concurrent recorder callbacks.
type ArtifactReceipts struct {
	mu       sync.Mutex
	receipts []ArtifactReceipt
}

func (r *ArtifactReceipts) Record(receipt ArtifactReceipt) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.receipts = append(r.receipts, receipt)
}

func (r *ArtifactReceipts) Snapshot() []ArtifactReceipt {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ArtifactReceipt(nil), r.receipts...)
}
