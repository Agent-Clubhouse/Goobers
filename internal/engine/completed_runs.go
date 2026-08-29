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
	projectOpts   []ProjectOption
	verified      map[string]bool
	nextPageToken []byte
}

// LiveJournalRegistry coordinates the reconciler with the daemon's live
// writer. Reserve takes exclusive control of a run for the duration of one
// reconcile pass: it refuses while the writer holds the run's journal open
// (Temporal may report the workflow closed while the final emission is still
// landing — such a run is never touched here), and while held it parks any
// emit for the run until release. A point-in-time IsOpen check is not enough
// (#3529): an emit can rehydrate the journal between the check and the
// backfill's directory replacement, leaving its acknowledged appends in an
// unlinked inode.
type LiveJournalRegistry interface {
	Reserve(runID string) (release func(), ok bool)
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

// WithLiveJournals attaches the daemon's live-writer registry so each run is
// reserved for the duration of its reconcile pass: runs the writer still
// holds open are skipped, and straggler emits cannot rehydrate a journal the
// pass is repairing (DS4/DS5). Optional: nil keeps the pre-live behavior.
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

// WithSpanSource attaches the same span source the live writer runs with, so
// the reconciler's BOTH projections — the repair/backfill write and the
// verification re-projection — can adopt an executor-recorded span by digest.
//
// Threading it into only one of the two is worse than threading it into
// neither (#3805). span.recorded is deliberately excluded from the
// conformance view (internal/journal/event.go), but the EventError that
// replaces an unadoptable span is NOT — its code is projected
// (internal/journal/conformance.go). So a live writer that adopts spans while
// the verification re-projection cannot produces a normative-view mismatch of
// exactly one event on every run carrying a transcript, and DS5 files a
// live_journal_divergence for each of them: a fleet-wide false alarm about
// blob-store reachability, reported as if the run itself disagreed with its
// own history. verify.go's DiffLiveJournal doc states the requirement; this
// is what honours it.
//
// Optional: a nil source leaves the reconciler exactly as it was, which is
// the correct shape for an instance whose live writer has no source either.
func (r *CompletedRunReconciler) WithSpanSource(src SpanSource) *CompletedRunReconciler {
	if src == nil {
		return r
	}
	r.projectOpts = []ProjectOption{WithSpanSource(src)}
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
		didProject, err := r.reconcileRun(ctx, runID, gaggle, runsDir, dir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if didProject {
			projected++
		}
	}
	return projected, errors.Join(errs...)
}

// reconcileRun verifies or repairs one closed run, holding the live writer's
// reservation for the whole pass so no emit can rehydrate the journal while
// the pass inspects — or replaces — its directory.
func (r *CompletedRunReconciler) reconcileRun(ctx context.Context, runID, gaggle, runsDir, dir string) (bool, error) {
	if r.live != nil {
		// Reserve, don't just peek: the live writer still owning this journal
		// (Temporal reports the workflow closed while the terminal emission is
		// landing) refuses the reservation and the run is skipped — the next
		// cycle finds the journal closed and proceeds. A successful
		// reservation parks any straggler emit until this pass is done, so
		// the backfill can never delete a directory an emit just rehydrated
		// into; the parked emit then re-derives state under the run-dir lock
		// and lands against whatever was published.
		release, ok := r.live.Reserve(runID)
		if !ok {
			return false, nil
		}
		defer release()
	}
	backfillingLive := false
	if journal.Recorded(dir) {
		inspection, err := inspectJournal(dir)
		if err != nil {
			return false, err
		}
		if inspection.complete {
			if inspection.liveAuthored && !r.verified[runID] {
				// DS5: the journal already exists from live authorship —
				// VERIFY against the independent history re-projection
				// instead of writing. A divergence is filed, never
				// silently repaired; the live journal stays the record.
				if err := r.verifyLiveRun(ctx, runID, gaggle, inspection.events); err != nil {
					return false, err
				}
				if r.verified == nil {
					r.verified = map[string]bool{}
				}
				r.verified[runID] = true
			}
			return false, observeProjectedRun(ctx, r.observe, runID, dir)
		}
		// An incomplete live-authored journal of a CLOSED run is the
		// crash-orphan case (the daemon died before the terminal emission
		// landed): backfilling it from history is exactly the repair role
		// DS5 retains — annotated below so the repair is visible, never
		// silent.
		backfillingLive = inspection.liveAuthored
	}
	if _, err := projectCompletedRun(ctx, r.client, runID, gaggle, runsDir, r.observe, r.spans, r.projectOpts...); err != nil {
		return false, err
	}
	if backfillingLive {
		r.reportDivergence(runID, "incomplete live-authored journal of a closed run was backfilled from the history re-projection")
	}
	return true, nil
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
	// The SAME projection options the live writer authored with — without
	// them a span the writer adopted re-projects as a span_unavailable error
	// event, which IS conformance-normative, and the diff reports an
	// environment difference as a run divergence (#3805).
	divergence, err := DiffLiveJournal(liveEvents, proj, r.projectOpts...)
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

func projectCompletedRun(ctx context.Context, q projectionQuerier, workflowID, gaggle, runsDir string, observe ProjectionObserver, spans SpanSink, opts ...ProjectOption) (string, error) {
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
	dir, err := ProjectCompletedRun(ctx, q, workflowID, runsDir, opts...)
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
