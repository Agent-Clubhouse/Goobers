package tutor

import (
	"context"
	"strconv"

	"go.opentelemetry.io/otel/attribute"

	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/providers"
)

// Recorder emits Tutor decisions back to telemetry.
type Recorder interface {
	RecordNoSignal(ctx context.Context, signalCount int)
	RecordProposal(ctx context.Context, proposal Proposal, pr providers.PullRequestResult)
}

// SpanRecorder records Tutor findings as events on an existing telemetry span.
type SpanRecorder struct {
	Span telemetry.Span
}

// RecordNoSignal records that the Tutor found no actionable signal.
func (r SpanRecorder) RecordNoSignal(_ context.Context, signalCount int) {
	r.Span.Event("tutor.no_signal", attribute.Int("tutor.signals", signalCount))
}

// RecordProposal records the generated proposal and resulting pull request.
func (r SpanRecorder) RecordProposal(_ context.Context, proposal Proposal, pr providers.PullRequestResult) {
	r.Span.Event("tutor.proposal",
		attribute.String("tutor.finding.type", string(proposal.Finding.Type)),
		attribute.String("tutor.workflow.id", proposal.Finding.WorkflowID),
		attribute.String("tutor.task.id", proposal.Finding.TaskID),
		attribute.String("tutor.gate.id", proposal.Finding.GateID),
		attribute.String("tutor.severity", proposal.Finding.Severity),
		attribute.String("tutor.rationale", proposal.Finding.Rationale),
		attribute.String("tutor.recommendation", proposal.Finding.Recommendation),
		attribute.String("tutor.pr.id", pr.ID),
		attribute.String("tutor.pr.url", pr.URL),
		attribute.Int("tutor.pr.number", pr.Number),
		attribute.Int("tutor.problem.count", proposal.Finding.ProblemCount),
	)
}

type noopRecorder struct{}

func (noopRecorder) RecordNoSignal(context.Context, int) {}
func (noopRecorder) RecordProposal(context.Context, Proposal, providers.PullRequestResult) {
}

func prID(pr providers.PullRequestResult) string {
	if pr.ID != "" {
		return pr.ID
	}
	if pr.Number > 0 {
		return strconv.Itoa(pr.Number)
	}
	return ""
}
