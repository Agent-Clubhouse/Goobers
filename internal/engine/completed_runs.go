package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/converter"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

const completedRunPageSize = 100

type completedRunClient interface {
	projectionQuerier
	ListWorkflow(context.Context, *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error)
}

// ProjectionObserver receives the terminal journal sequence after a projection
// is durably published.
type ProjectionObserver func(context.Context, string, uint64) error

// CompletedRunReconciler is the repair/backfill projection over closed
// Temporal engine runs (DS5 — demoted from journal authority). A run whose
// journal was live-authored (DS4) is VERIFIED against a history
// re-projection, with divergences filed through the divergence reporter —
// never silently repaired; a run with no live journal (pre-upgrade history,
// or a run started without live journaling) still projects exactly as
// before. Reconcile is bounded to one visibility page; successive calls
// continue pagination and cycle back to the newest page.
type CompletedRunReconciler struct {
	client        completedRunClient
	namespace     string
	runsDirs      map[string]string
	observe       ProjectionObserver
	spans         SpanSink
	live          LiveJournalRegistry
	report        DivergenceReporter
	verified      map[string]bool
	nextPageToken []byte
}

// LiveJournalRegistry answers whether the daemon's live writer currently
// holds a run's journal open. Such a run is never touched here: Temporal may
// report the workflow closed while the final emission is still landing.
type LiveJournalRegistry interface {
	IsOpen(runID string) bool
}

// DivergenceReporter files one live-vs-projected divergence (or repair
// annotation) into the named channel the #2871 parity ledger reads — the
// daemon wires an instance-journal error event with a stable code.
type DivergenceReporter func(runID, detail string)

// WithSpans attaches a span sink, so each newly projected run also emits
// backdated telemetry (#2865). Optional: a nil sink leaves the reconciler
// exactly as it was, which is the shape for an instance with telemetry
// disabled.
func (r *CompletedRunReconciler) WithSpans(sink SpanSink) *CompletedRunReconciler {
	r.spans = sink
	return r
}

// WithLiveJournals attaches the daemon's live-writer registry so runs the
// writer still holds open are skipped (DS4/DS5). Optional: nil keeps the
// pre-live behavior.
func (r *CompletedRunReconciler) WithLiveJournals(live LiveJournalRegistry) *CompletedRunReconciler {
	r.live = live
	return r
}

// WithDivergenceReporter attaches the parity channel divergences are filed
// to. Optional: without one, divergences surface only as reconcile errors.
func (r *CompletedRunReconciler) WithDivergenceReporter(report DivergenceReporter) *CompletedRunReconciler {
	r.report = report
	return r
}

// reportDivergence files into the named channel, falling back to nothing when
// none is wired (the caller has already decided the reconcile outcome).
func (r *CompletedRunReconciler) reportDivergence(runID, detail string) {
	if r.report != nil {
		r.report(runID, detail)
	}
}

// NewCompletedRunReconciler constructs a reconciler scoped to configured
// gaggle names and their journal roots.
func NewCompletedRunReconciler(c completedRunClient, namespace string, runsDirs map[string]string, observe ProjectionObserver) (*CompletedRunReconciler, error) {
	if c == nil {
		return nil, errors.New("engine: Temporal client is required")
	}
	if namespace == "" {
		return nil, errors.New("engine: Temporal namespace is required")
	}
	if len(runsDirs) == 0 {
		return nil, errors.New("engine: at least one gaggle runs directory is required")
	}
	return &CompletedRunReconciler{client: c, namespace: namespace, runsDirs: runsDirs, observe: observe}, nil
}

// Reconcile processes one bounded page of closed workflow executions.
func (r *CompletedRunReconciler) Reconcile(ctx context.Context) (int, error) {
	response, err := r.client.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
		Namespace:     r.namespace,
		PageSize:      completedRunPageSize,
		NextPageToken: r.nextPageToken,
		Query:         "ExecutionStatus != 'Running'",
	})
	if err != nil {
		return 0, fmt.Errorf("engine: list completed workflows: %w", err)
	}
	r.nextPageToken = append(r.nextPageToken[:0], response.NextPageToken...)

	var (
		projected int
		errs      []error
	)
	for _, info := range response.Executions {
		memo := info.GetMemo().GetFields()[RunGaggleMemoKey]
		if memo == nil {
			continue
		}
		var gaggle string
		if err := converter.GetDefaultDataConverter().FromPayload(memo, &gaggle); err != nil {
			errs = append(errs, fmt.Errorf("engine: decode gaggle memo for %q: %w", info.GetExecution().GetWorkflowId(), err))
			continue
		}
		runsDir, ok := r.runsDirs[gaggle]
		if !ok {
			continue
		}
		runID := info.GetExecution().GetWorkflowId()
		if runID == "" {
			errs = append(errs, errors.New("engine: completed workflow has no workflow id"))
			continue
		}
		dir, err := completedRunDir(runsDir, runID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		// The live writer still owns this journal: even though Temporal
		// reports the workflow closed, the terminal emission may be landing
		// right now. Touching the directory here would race the single
		// writer; the next cycle finds the journal closed and proceeds.
		if r.live != nil && r.live.IsOpen(runID) {
			continue
		}
		backfillingLive := false
		if journal.Recorded(dir) {
			inspection, err := inspectJournal(dir)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if inspection.complete {
				if inspection.liveAuthored && !r.verified[runID] {
					// DS5: the journal already exists from live authorship —
					// VERIFY against the independent history re-projection
					// instead of writing. A divergence is filed, never
					// silently repaired; the live journal stays the record.
					if err := r.verifyLiveRun(ctx, runID, gaggle, inspection.events); err != nil {
						errs = append(errs, err)
						continue
					}
					if r.verified == nil {
						r.verified = map[string]bool{}
					}
					r.verified[runID] = true
				}
				if err := observeProjectedRun(ctx, r.observe, runID, dir); err != nil {
					errs = append(errs, err)
				}
				continue
			}
			// An incomplete live-authored journal of a CLOSED run is the
			// crash-orphan case (the daemon died before the terminal emission
			// landed): backfilling it from history is exactly the repair role
			// DS5 retains — annotated below so the repair is visible, never
			// silent.
			backfillingLive = inspection.liveAuthored
		}
		if _, err := projectCompletedRun(ctx, r.client, runID, gaggle, runsDir, r.observe, r.spans); err != nil {
			errs = append(errs, err)
			continue
		}
		if backfillingLive {
			r.reportDivergence(runID, "incomplete live-authored journal of a closed run was backfilled from the history re-projection")
		}
		projected++
	}
	return projected, errors.Join(errs...)
}

// verifyLiveRun diffs a complete live-authored journal's normative view
// against the history re-projection (DS5's conformance cross-check). A
// divergence is filed through the reporter; inability to verify (the query
// failed, the history is unprojectable) is an error so the next cycle
// retries.
func (r *CompletedRunReconciler) verifyLiveRun(ctx context.Context, runID, gaggle string, liveEvents []journal.Event) error {
	proj, err := queryProjection(ctx, r.client, runID)
	if err != nil {
		return fmt.Errorf("engine: verify live journal for %q: %w", runID, err)
	}
	if proj.Identity.RunID != runID || proj.Identity.Gaggle != gaggle {
		r.reportDivergence(runID, fmt.Sprintf(
			"history projects identity %s/%s, journal directory is %s/%s",
			proj.Identity.Gaggle, proj.Identity.RunID, gaggle, runID))
		return nil
	}
	divergence, err := DiffLiveJournal(liveEvents, proj)
	if err != nil {
		return fmt.Errorf("engine: verify live journal for %q: %w", runID, err)
	}
	if divergence != "" {
		r.reportDivergence(runID, divergence)
	}
	return nil
}

// ProjectCompletedRunForGaggle is the manual projection path with the same
// gaggle validation and read-model notification used by the reconciler.
func ProjectCompletedRunForGaggle(ctx context.Context, q projectionQuerier, workflowID, gaggle, runsDir string, observe ProjectionObserver) (string, error) {
	dir, err := completedRunDir(runsDir, workflowID)
	if err != nil {
		return "", err
	}
	if journal.Recorded(dir) {
		complete, err := projectedJournalComplete(dir)
		if err != nil {
			return "", err
		}
		if complete {
			return dir, observeProjectedRun(ctx, observe, workflowID, dir)
		}
	}
	// No span sink on the manual path. `goobers engine-project` is a one-shot
	// command that builds no telemetry client, and a run projected by hand is
	// almost always being recovered rather than observed. The daemon's
	// reconciler is where spans are emitted, because that is the path every run
	// takes automatically.
	return projectCompletedRun(ctx, q, workflowID, gaggle, runsDir, observe, nil)
}

func completedRunDir(runsDir, workflowID string) (string, error) {
	if !apiv1.ValidRunID(workflowID) {
		return "", fmt.Errorf("%w: workflow id %q is not a safe path segment", ErrUnprojectable, workflowID)
	}
	return filepath.Join(runsDir, workflowID), nil
}

func projectCompletedRun(ctx context.Context, q projectionQuerier, workflowID, gaggle, runsDir string, observe ProjectionObserver, spans SpanSink) (string, error) {
	proj, err := queryProjection(ctx, q, workflowID)
	if err != nil {
		return "", err
	}
	if proj.Identity.RunID != workflowID {
		return "", fmt.Errorf("%w: workflow id %q projected run id %q", ErrUnprojectable, workflowID, proj.Identity.RunID)
	}
	if proj.Identity.Gaggle != gaggle {
		return "", fmt.Errorf("%w: workflow %q memo gaggle %q does not match projected gaggle %q", ErrUnprojectable, workflowID, gaggle, proj.Identity.Gaggle)
	}
	dir, err := ProjectCompletedRun(ctx, q, workflowID, runsDir)
	if err != nil {
		return "", err
	}
	if err := observeProjectedRun(ctx, observe, workflowID, dir); err != nil {
		return "", err
	}
	// Telemetry is emitted AFTER the journal is durable, and its failure does
	// not fail the projection: a run whose journal was written correctly has not
	// failed because a span could not be exported. The reverse would trade the
	// durable record for the observability of it.
	if err := SynthesizeRunSpans(ctx, spans, proj); err != nil {
		return dir, nil
	}
	return dir, nil
}

func observeProjectedRun(ctx context.Context, observe ProjectionObserver, workflowID, dir string) error {
	if observe == nil {
		return nil
	}
	rd, err := journal.OpenRead(dir)
	if err != nil {
		return err
	}
	events, err := rd.Events()
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return fmt.Errorf("%w: projected journal has no events", ErrUnprojectable)
	}
	if err := observe(ctx, workflowID, events[len(events)-1].Seq); err != nil {
		return fmt.Errorf("engine: notify projected run %q: %w", workflowID, err)
	}
	return nil
}
