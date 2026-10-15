package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

// importRun reads a real run directory into an eval case skeleton: the
// journal and recorded span content attribution reads, plus label provenance
// naming the source run. When the run carries a ground-truth label store
// (`goobers telemetry label`), its effective label's labeler, time, and
// reason seed the provenance. The store records only whether the run's
// attribution was correct, so the expected domain, stage, step, and class are
// left for the labeler to fill in.
func importRun(runDir string) (evalCase, error) {
	reader, err := journal.OpenReadOnly(runDir)
	if err != nil {
		return evalCase{}, err
	}
	identity, err := reader.Identity()
	if err != nil {
		return evalCase{}, err
	}
	records, err := reader.EventRecords()
	if err != nil {
		return evalCase{}, err
	}
	input := evalInput{
		RunID: identity.RunID, Gaggle: identity.Gaggle, Workflow: identity.Workflow,
		Events: make([]journal.Event, len(records)),
	}
	for i := range records {
		input.Events[i] = records[i].Event
		if err := importSpan(reader, &input, records[i].Event); err != nil {
			return evalCase{}, err
		}
	}
	provenance := &labelProvenance{SourceRun: identity.RunID}
	labels, err := creditgraph.ReadLabelStore(runDir)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return evalCase{}, fmt.Errorf("read ground-truth labels: %w", err)
	default:
		if label, ok := creditgraph.EffectiveLabel(labels.Labels); ok {
			provenance.LabeledBy = label.LabeledBy
			provenance.LabeledAt = label.LabeledAt.UTC().Format("2006-01-02")
			provenance.Reason = label.Reason
		}
	}
	return evalCase{
		ID:       "real-" + identity.RunID,
		Input:    input,
		Expected: evalLabel{Source: labelHuman, Workflow: identity.Workflow, Provenance: provenance},
	}, nil
}

func importSpan(reader *journal.Reader, input *evalInput, event journal.Event) error {
	if event.Type != journal.EventSpanRecorded || event.Ref == nil ||
		(event.DataSchema != telemetry.GenAIEventSchema && event.DataSchema != telemetry.SpanSchema) {
		return nil
	}
	data, err := reader.SpanBytes(*event.Ref)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("read span at sequence %d: %w", event.Seq, err)
	}
	if input.SpanData == nil {
		input.SpanData = map[string]string{}
	}
	input.SpanData[event.Ref.Digest] = string(data)
	return nil
}
