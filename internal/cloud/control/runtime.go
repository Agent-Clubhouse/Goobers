package control

import (
	"context"
	"errors"
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
	Report        func(error)
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
		next, err := r.drain(ctx, store, starter, after)
		after = next
		return err
	}
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
		return reconcile(ctx)
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
