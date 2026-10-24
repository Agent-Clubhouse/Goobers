package control

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/client"

	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/podauth"
	"github.com/goobers/goobers/internal/cloud/startfence"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Runtime provides the concrete acceptance/start loop and the required seams
// for stores and engine lifecycle services that land independently of control.
// The factory must produce callbacks bound to this epoch, never a later owner.
type Runtime struct {
	Configuration Configuration
	Pool          *pgxpool.Pool
	InstanceID    string
	Lease         *lease.Manager
	Temporal      client.Client
	TaskQueue     string
	Resolve       acceptance.StartInputResolver
	Services      func(context.Context, *lease.Handle) (EpochServices, error)
	// Admission, when set, replaces the legacy direct-start drain: accepted
	// rows are admitted through the scheduler's run conditions instead of
	// starting straight at the start fence. Nil keeps the legacy drain.
	Admission func(context.Context, AdmissionDeps) (Admission, error)
	Report    func(error)
}

// AdmissionDeps are the epoch-bound pieces an Admission is built from. Ctx (the
// Build context) is the epoch context: everything the Admission starts must end
// with it.
type AdmissionDeps struct {
	Handle  *lease.Handle
	Store   *acceptance.Store
	Starter engine.Starter
	Resolve func(context.Context, triggerqueue.Record) (engine.RunInput, error)
}

// Admission is one epoch's scheduler-backed admission of accepted starts.
type Admission interface {
	// Prepare rebuilds admission from durable state (reconcile the scheduler,
	// watch reconciled predecessor runs). It runs after uncertain starts are
	// reconciled and before the epoch serves.
	Prepare(context.Context) error
	// Tick runs one scheduler pass and admits accepted rows.
	Tick(context.Context) error
	// Stop ends the epoch's admission goroutines and waits for them.
	Stop(context.Context) error
}

// Build creates a fresh acceptance adapter and starter for one acquired lease.
func (r Runtime) Build(ctx context.Context, handle *lease.Handle) (EpochServices, error) {
	if r.Configuration == nil || r.Pool == nil || r.Lease == nil || r.Temporal == nil || r.Resolve == nil || r.Services == nil || r.TaskQueue == "" {
		return EpochServices{}, errors.New("cloud control requires pinned input, lifecycle services and Temporal wiring")
	}
	services, err := r.Services(ctx, handle)
	if err != nil {
		return services, err
	}
	if err := services.validate(); err != nil {
		return services, err
	}
	store, err := acceptance.New(r.Pool, r.InstanceID)
	if err != nil {
		return services, err
	}
	store.BindFence(r.Lease, handle)
	if services.RestoreObservations == nil || services.CaptureObservations == nil {
		return services, errors.New("cloud control requires durable provider observation wiring")
	}
	observations := Observations{Pool: r.Pool, InstanceID: r.InstanceID, Lease: r.Lease, Handle: handle}
	rebuild := services.RebuildBeforeReap
	services.RebuildBeforeReap = func(ctx context.Context) error {
		if err := r.Configuration.Reload(ctx); err != nil {
			return err
		}
		snapshot, err := observations.Load(ctx)
		if err != nil {
			return err
		}
		if err := services.RestoreObservations(ctx, snapshot); err != nil {
			return err
		}
		return rebuild(ctx)
	}

	starter := startfence.New(r.Temporal, r.TaskQueue, acceptance.NewStartAdmission(store), r.Configuration)
	reconcile := func(ctx context.Context) error { return store.RecoverStarts(ctx, starter, r.resolve) }
	originalReconcile, originalL1, originalL2 := services.ReconcileUncertain, services.L1, services.L2
	services.ReconcileUncertain = func(ctx context.Context) error {
		if err := originalReconcile(ctx); err != nil {
			return err
		}
		return reconcile(ctx)
	}
	var admission Admission
	if r.Admission != nil {
		admission, err = r.Admission(ctx, AdmissionDeps{Handle: handle, Store: store, Starter: starter, Resolve: r.resolve})
		if err != nil {
			return services, err
		}
		originalAttach, originalStop := services.ReattachAwaiters, services.Stop
		services.ReattachAwaiters = func(ctx context.Context) error {
			if err := originalAttach(ctx); err != nil {
				return err
			}
			return admission.Prepare(ctx)
		}
		services.Stop = func(ctx context.Context) error {
			return errors.Join(originalStop(ctx), admission.Stop(ctx))
		}
	}
	var after string
	services.L1 = func(ctx context.Context) error {
		if err := r.Configuration.Reload(ctx); err != nil {
			return err
		}
		if err := originalL1(ctx); err != nil {
			return err
		}
		snapshot, err := services.CaptureObservations(ctx)
		if err != nil {
			return err
		}
		if err := observations.Replace(ctx, snapshot); err != nil {
			return err
		}
		if admission != nil {
			return admission.Tick(ctx)
		}
		next, err := r.drain(ctx, store, starter, after)
		after = next
		return err
	}
	pins := runPinReports{}
	services.L2 = func(ctx context.Context) error {
		if err := store.Prune(ctx); err != nil {
			return err
		}
		if err := podauth.PruneExpired(ctx, r.Lease, handle); err != nil {
			return err
		}
		if err := originalL2(ctx); err != nil {
			return err
		}
		if err := reconcile(ctx); err != nil {
			return err
		}
		// Run pins outlive their acceptance row: they are retained until the
		// run's engine execution is closed, then for ReplayRetention more. The
		// sweep is best-effort against Temporal: a run it cannot vouch for keeps
		// its pins and is reported, never returned, since an L2 error ends every
		// loop and drops the epoch; its Temporal calls are bounded by
		// runPinSweepBudget so an outage cannot hold up claim renewal and reap.
		// Only the store's own errors (a lost fence among them) are returned, as
		// for Prune above.
		sweep, err := store.SweepRunPins(ctx, temporalRunClosed(r.Temporal), pins.after, runPinSweepBudget)
		pins.observe(sweep, r.report)
		return err
	}
	return services, nil
}

func (r Runtime) resolve(ctx context.Context, record triggerqueue.Record) (engine.RunInput, error) {
	in, err := r.Resolve(ctx, record)
	if err != nil {
		return engine.RunInput{}, err
	}
	if !in.SelfExecutionDenied {
		return engine.RunInput{}, errors.New("cloud accepted input must pin selfExecution: deny")
	}
	in.AcceptanceID = record.ID
	return in, nil
}

func (r Runtime) drain(ctx context.Context, store *acceptance.Store, starter engine.Starter, after string) (string, error) {
	// Page across retained custody so an invalid or uncertain item cannot starve
	// the next accepted workflow. Temporal identity is always supplied by Store.
	rows, err := store.RetainedPage(ctx, after, 100)
	if err != nil {
		return after, err
	}
	var workers sync.WaitGroup
	slots := make(chan struct{}, 4)
	for _, record := range rows {
		after = record.ID
		if record.State != triggerqueue.Accepted {
			continue
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			workers.Wait()
			return after, ctx.Err()
		}
		workers.Go(func() {
			defer func() { <-slots }()
			attempt, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			in, err := r.resolve(attempt, record)
			if errors.Is(err, acceptance.ErrNotOrdinaryStart) {
				// A generated child's custody is owned by its own lifecycle. A
				// payload that is not even a readable kind is corrupt and nothing
				// else will ever look at it, so it is reported (still never rejected).
				if errors.Is(err, acceptance.ErrCorruptStart) && r.Report != nil {
					r.Report(fmt.Errorf("accepted row %s: %w", record.ID, err))
				}
				return
			}
			if errors.Is(err, acceptance.ErrPermanentStart) {
				// Retrying can never succeed and would re-pin the generation on
				// every sweep: finish the row as rejected with the reason.
				err = errors.Join(err, store.RejectAccepted(attempt, record.ID, err.Error(), time.Now()))
			} else if err == nil {
				_, err = starter.Start(attempt, in)
			}
			if err != nil && r.Report != nil {
				r.Report(err)
			}
		})
	}
	workers.Wait()

	if len(rows) < 100 {
		after = ""
	}
	return after, nil
}

func (r Runtime) report(err error) {
	if r.Report != nil {
		r.Report(err)
	}
}

// runPinSweepBudget bounds one L2 pass's Temporal calls for the run-pin sweep.
var runPinSweepBudget = 5 * time.Second

// runPinHaltSkip is how many consecutive passes may halt at the same run
// before the cursor steps past it. A run whose Describe always outlasts the
// budget would otherwise stall retention for every run behind it.
const runPinHaltSkip = 3

// runPinReports carries the sweep cursor across L2 passes and reports each
// unconfirmed run, and a Temporal outage, once when it starts rather than on
// every pass while it lasts. A run the sweep keeps halting at is skipped
// (reported, its pins kept) and retried on the next lap.
type runPinReports struct {
	after       string
	unconfirmed map[string]bool
	halted      bool
	haltedAt    string
	halts       int
}

func (p *runPinReports) observe(sweep acceptance.RunPinSweep, report func(error)) {
	p.after = sweep.Next
	switch sweep.HaltedAt {
	case "":
		p.haltedAt, p.halts = "", 0
	case p.haltedAt:
		p.halts++
	default:
		p.haltedAt, p.halts = sweep.HaltedAt, 1
	}
	if p.halts >= runPinHaltSkip {
		report(fmt.Errorf("run pin sweep: skipping run %s after %d passes halted at it; its pins are kept: %w", p.haltedAt, p.halts, sweep.Halted))
		p.after = p.haltedAt
		p.haltedAt, p.halts = "", 0
	}
	p.report(sweep, report)
}

func (p *runPinReports) report(sweep acceptance.RunPinSweep, report func(error)) {
	if p.unconfirmed == nil {
		p.unconfirmed = map[string]bool{}
	}
	for _, runID := range sweep.Checked {
		delete(p.unconfirmed, runID)
	}
	for runID, err := range sweep.Unconfirmed {
		if !p.unconfirmed[runID] {
			p.unconfirmed[runID] = true
			report(err)
		}
	}
	if sweep.Halted != nil && !p.halted {
		report(sweep.Halted)
	}
	p.halted = sweep.Halted != nil
}
