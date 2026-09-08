package readservice

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/prqueue"
	"github.com/goobers/goobers/internal/readmodel"
)

// QueueEligibilityView is historical selection evidence for one workflow.
// Observed never means currently claimable: consumers must show the report's
// observation time and the projection read state alongside its decisions.
type QueueEligibilityView struct {
	ReadStateEnvelope
	Gaggle      string          `json:"gaggle"`
	Workflow    string          `json:"workflow"`
	AsOf        time.Time       `json:"asOf"`
	Status      string          `json:"status"`
	SourceRunID string          `json:"sourceRunId,omitempty"`
	SourceStage string          `json:"sourceStage,omitempty"`
	Report      *prqueue.Report `json:"report,omitempty"`
	Problem     string          `json:"problem,omitempty"`
}

// QueueEligibility reads one indexed scoped observation and its verified blob.
// It does not replay events, refresh providers, or mutate claims. Callers that
// expose this method remotely must authorize the gaggle before invoking it.
func (s *Local) QueueEligibility(ctx context.Context, gaggle, workflow string) (QueueEligibilityView, error) {
	if err := ctx.Err(); err != nil {
		return QueueEligibilityView{}, err
	}
	if workflow == "" || len(workflow) > 256 || len(gaggle) > 256 || strings.ContainsAny(gaggle+workflow, "/\\") || gaggle == "." || gaggle == ".." {
		return QueueEligibilityView{}, fmt.Errorf("%w: invalid queue scope", ErrInvalidArgument)
	}
	view := QueueEligibilityView{ReadStateEnvelope: s.readStateEnvelope(ctx), Gaggle: gaggle, Workflow: workflow, AsOf: s.now().UTC(), Status: "unavailable"}
	reader, ok := s.sources.ReadModel.(readmodel.QueueEligibilityReader)
	if !ok || !s.readModelReads {
		view.Problem = "Queue eligibility projection is unavailable."
		return view, nil
	}
	state, err := s.sources.ReadModel.State(ctx)
	if err != nil {
		return view, err
	}
	if !state.Ready {
		view.Problem = "Queue eligibility projection is rebuilding."
		return view, nil
	}
	row, found, err := reader.LatestQueueEligibility(ctx, gaggle, workflow)
	if err != nil {
		return view, err
	}
	if !found {
		view.Status = "not-observed"
		view.Problem = "No retained queue eligibility observation exists for this workflow."
		return view, nil
	}
	view.SourceRunID, view.SourceStage = row.RunID, row.Evidence.Stage
	if row.Evidence.Problem != "" {
		view.Problem = row.Evidence.Problem
		return view, nil
	}
	report, err := s.readQueueEligibilityReport(ctx, gaggle, workflow, row)
	if err != nil {
		if ctx.Err() != nil {
			return view, ctx.Err()
		}
		view.Problem = "Recorded queue eligibility evidence could not be verified."
		return view, nil
	}
	view.Status, view.Report = "observed", report
	return view, nil
}

func (s *Local) readQueueEligibilityReport(ctx context.Context, gaggle, workflow string, row readmodel.QueueEligibilityRow) (*prqueue.Report, error) {
	if !apiv1.ValidRunID(row.RunID) || row.Evidence.Artifact == nil {
		return nil, fmt.Errorf("invalid queue evidence pointer")
	}
	dir, err := s.sources.Layout.FindRunDir(row.RunID)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("invalid run directory")
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		return nil, err
	}
	identity, err := reader.Identity()
	if err != nil {
		return nil, err
	}
	if identity.RunID != row.RunID || identity.Gaggle != gaggle || identity.Workflow != workflow {
		return nil, fmt.Errorf("queue journal ownership mismatch")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := reader.ArtifactBytesBounded(*row.Evidence.Artifact, prqueue.MaxReportBytes+(64<<10))
	if err != nil {
		return nil, err
	}
	var result struct {
		Report prqueue.Report `json:"queueEligibility"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	r := &result.Report
	if r.RunID != row.RunID || r.Gaggle != gaggle || r.Workflow != workflow || r.RepositoryKey != row.Evidence.RepositoryKey || !r.ObservedAt.Equal(row.Evidence.ObservedAt) {
		return nil, fmt.Errorf("queue artifact ownership or projection mismatch")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r, nil
}
