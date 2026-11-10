package schedstart

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

const (
	pageSize = 100
	// defaultDispatchTimeout bounds scheduler admission for one record. The
	// run itself continues on the epoch context, not on this one.
	defaultDispatchTimeout = 10 * time.Second
	defaultConcurrency     = 4
)

// Pager reads retained acceptance custody in id order.
type Pager interface {
	RetainedPage(ctx context.Context, after string, limit int) ([]triggerqueue.Record, error)
}

// Drain hands accepted rows to the scheduler. Every dispatch goroutine ends
// before Run returns; the runs themselves are owned by the scheduler on the
// epoch context passed as ctx.
type Drain struct {
	Pager Pager
	// Service is the template startintent service (Queue, Build, Scheduler,
	// Now). Its Build is wrapped per record so the entry's Starter is the
	// record-bound cloud Starter. RunDirectory must return "": the cloud
	// proves a prior start through Temporal (AlreadyRunning), not a local
	// journal directory.
	Service  startintent.Service
	Config   Config
	Tracker  *Tracker
	Parallel int
	Timeout  time.Duration
	Report   func(error)
	// RejectAccepted must atomically refuse only an unstarted ordinary row,
	// under the epoch fence. It must never reject dispatching or child custody.
	RejectAccepted func(context.Context, string, string, time.Time) error

	// dispatch overrides the service dispatch in tests.
	dispatch func(admission, execution context.Context, record triggerqueue.Record) error
}

// Run dispatches one page and returns the cursor to resume from.
func (d Drain) Run(ctx context.Context, after string) (string, error) {
	rows, err := d.Pager.RetainedPage(ctx, after, pageSize)
	if err != nil {
		return after, err
	}
	parallel, timeout := d.Parallel, d.Timeout
	if parallel < 1 {
		parallel = defaultConcurrency
	}
	if timeout <= 0 {
		timeout = defaultDispatchTimeout
	}
	slots := make(chan struct{}, parallel)
	var workers sync.WaitGroup
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
			admission, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			if err := d.dispatchOne(admission, ctx, record); err != nil && d.Report != nil {
				d.Report(err)
			}
		})
	}
	workers.Wait()
	if len(rows) < pageSize {
		after = ""
	}
	return after, nil
}

// bind returns a service whose prepared entry runs record's cloud Starter.
func (d Drain) bind(record triggerqueue.Record) *startintent.Service {
	service := d.Service
	build := service.Build
	service.Build = func(ctx context.Context, target startintent.Target) (startintent.Prepared, error) {
		prepared, err := build(ctx, target)
		if err != nil {
			return prepared, err
		}
		prepared.Entry.Starter = d.Config.StarterFor(record, d.Tracker)
		return prepared, nil
	}
	return &service
}

func (d Drain) dispatchOne(admission, execution context.Context, record triggerqueue.Record) error {
	if d.dispatch != nil {
		return d.dispatch(admission, execution, record)
	}
	// Resolve before BeginDispatch: resolver failures prove no engine call was
	// made. After dispatch, failures are uncertain and must retain custody.
	in, err := d.Config.Resolve(admission, record)
	if err != nil {
		return d.refuse(admission, record, err)
	}
	// Keep the resolved input bound to this acceptance. The scheduler still
	// admits the run before invoking the engine, without resolving a second time
	// after the row has crossed into uncertain custody.
	d.Config.Resolve = func(context.Context, triggerqueue.Record) (engine.RunInput, error) {
		return in, nil
	}
	return d.bind(record).Dispatch(admission, execution, record)
}

func (d Drain) refuse(ctx context.Context, record triggerqueue.Record, err error) error {
	if errors.Is(err, acceptance.ErrCorruptStart) {
		return err
	}
	if errors.Is(err, acceptance.ErrNotOrdinaryStart) {
		return nil
	}
	if errors.Is(err, acceptance.ErrPermanentStart) {
		if d.RejectAccepted == nil {
			return errors.Join(err, errors.New("schedstart: accepted rejection unavailable"))
		}
		return errors.Join(err, d.RejectAccepted(ctx, record.ID, err.Error(), d.Service.Now()))
	}
	return err // transient: leave Accepted for the next sweep
}
