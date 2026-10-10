package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func installOneShotSignalQueue(layout instance.Layout, setup *schedulerSetup) (*durableTriggerService, error) {
	service, err := newDurableTriggerService(filepath.Join(layout.SchedulerDir(), "accepted-triggers.db"), newDaemonTriggerService())
	if err != nil {
		return nil, err
	}
	service.auditLog = setup.InstanceLog
	if err = setup.installOrdinaryStarts(layout, service); err != nil {
		_ = service.queue.Close()
		return nil, err
	}
	return service, nil
}

// Only this invocation's accepted recipients are drained by the one-shot CLI.
// Capacity-held starts remain durable for the daemon; unrelated queue records
// never execute as a side effect of delivering a named signal.
func dispatchQueuedSignal(ctx context.Context, service *durableTriggerService, sched *localscheduler.Scheduler, key, name string, output io.Writer) ([]string, bool, error) {
	service.dispatch.AttachScheduler(sched)
	service.dispatch.AttachDispatchContext(ctx)
	ids, err := sched.QueueSignal(ctx, key, name, time.Now())
	if err != nil {
		return nil, false, err
	}
	var runs []string
	pending := false
	for _, id := range ids {
		record, err := service.queue.Get(ctx, id, "local-signal")
		if err != nil {
			return runs, pending, fmt.Errorf("accepted signal receipt %s unavailable: %w", id, err)
		}
		if record.State == triggerqueue.Dispatching {
			if err = service.reconcileAcceptedRecord(ctx, record); err != nil {
				return runs, pending, err
			}
			record, err = service.queue.Get(ctx, id, "local-signal")
			if err != nil {
				return runs, pending, err
			}
		}
		if record.State == triggerqueue.Accepted {
			if err = service.drainOrdinary(ctx, record); err != nil {
				return runs, pending, err
			}
			record, err = service.queue.Get(ctx, id, "local-signal")
			if err != nil {
				return runs, pending, err
			}
		}
		switch record.State {
		case triggerqueue.Accepted:
			pending = true
			pf(output, "queued start %s (signal=%s); start goobers up to drain pending capacity\n", id, name)
		case triggerqueue.Rejected:
			return runs, pending, fmt.Errorf("signal start %s rejected: %s", id, record.Reason)
		case triggerqueue.Dispatching, triggerqueue.Dispatched:
			if record.RunID == "" {
				return runs, pending, errors.New("signal start publication uncertain; retry the same --request-id")
			}
			if err := awaitSignalPublication(ctx, service, record); err != nil {
				return runs, pending, err
			}
			runs = append(runs, strings.TrimPrefix(id, "trigger-"))
		}
	}
	return runs, pending, nil
}

// A reserved scheduler ID is not yet proof that a run was created.
func awaitSignalPublication(ctx context.Context, service *durableTriggerService, record triggerqueue.Record) error {
	if record.State == triggerqueue.Dispatched {
		return nil
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		observed, err := service.ordinary.Observe(bounded, record)
		if err != nil {
			return err
		}
		if observed {
			return service.queue.Finish(bounded, record.ID, triggerqueue.Dispatched, strings.TrimPrefix(record.ID, "trigger-"), "", time.Now())
		}
		select {
		case <-bounded.Done():
			return fmt.Errorf("signal receipt %s publication remains uncertain; retry the same --request-id: %w", record.ID, bounded.Err())
		case <-ticker.C:
		}
	}
}
