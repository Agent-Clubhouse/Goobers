package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func installOneShotStartQueue(layout instance.Layout, setup *schedulerSetup) (*durableTriggerService, error) {
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

// dispatchStandaloneStart enters the same durable queue as the daemon and only
// attempts this caller's receipt. Holding the instance lock proves that prior
// local dispatch custody belongs to an earlier owner, never another live CLI.
func dispatchStandaloneStart(ctx context.Context, layout instance.Layout, setup *schedulerSetup, sched *localscheduler.Scheduler, target runTarget, output io.Writer) (runID string, resultErr error) {
	key := strings.TrimSpace(target.RequestID)
	if key == "" {
		var err error
		key, err = newRemoteTriggerRequestID()
		if err != nil {
			return "", err
		}
	}
	service, err := installOneShotStartQueue(layout, setup)
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, service.queue.Close()) }()
	service.dispatch.AttachScheduler(sched)
	service.dispatch.AttachDispatchContext(ctx)
	request := startintent.Request{Workflow: target.Workflow, Gaggle: target.Gaggle, Force: target.Force, PullRequest: target.PR}
	record, _, err := service.ordinary.Accept(ctx, key, "standalone-cli", request)
	if err != nil {
		return "", fmt.Errorf("start request %q: %w; retry with the same --request-id and options", key, err)
	}
	if record.State == triggerqueue.Dispatching {
		if err = service.reconcileAcceptedRecord(ctx, record); err != nil {
			return "", standaloneReceiptError(output, record, key, err)
		}
		record, err = service.queue.Get(ctx, record.ID, "standalone-cli")
		if err != nil {
			return "", err
		}
	}
	if record.State == triggerqueue.Accepted {
		if err = service.drainOrdinary(ctx, record); err != nil {
			return "", standaloneReceiptError(output, record, key, err)
		}
		record, err = service.queue.Get(ctx, record.ID, "standalone-cli")
		if err != nil {
			return "", err
		}
	}
	if record.State == triggerqueue.Rejected {
		return "", standaloneReceiptError(output, record, key, errors.New(record.Reason))
	}
	if record.RunID == "" {
		return "", standaloneReceiptError(output, record, key, errors.New("start remains queued; start goobers up to drain pending capacity, or retry with the same --request-id"))
	}
	return record.RunID, nil
}
func standaloneReceiptError(output io.Writer, record triggerqueue.Record, key string, err error) error {
	pf(output, "accepted trigger %s (request=%s, state=%s)\n", record.ID, key, record.State)
	return fmt.Errorf("trigger %s (request %q): %w", record.ID, key, err)
}

func detachedSelectorWithRequest(name, key string) string {
	if key == "" {
		return name
	}
	return name + "#request-" + base64.RawURLEncoding.EncodeToString([]byte(key))
}

// Allocate the identity before spawning, so an interrupted parent can report
// the retry key even if the child already committed custody without replying.
func runDetachedQueuedTrigger(ctx context.Context, layout instance.Layout, target runTarget, root string, stdout, stderr io.Writer) int {
	if strings.TrimSpace(target.RequestID) == "" {
		key, err := newRemoteTriggerRequestID()
		if err != nil {
			pf(stderr, "error: create start request identity: %v\n", err)
			return 2
		}
		target.RequestID = key
	}
	pf(stdout, "start request %q (retry an unknown outcome with the same --request-id and options)\n", target.RequestID)
	return runDetachedTrigger(ctx, layout, detachedRunSelector(target), root, stdout, stderr)
}
func splitDetachedRequest(name string) (string, string, error) {
	marker := strings.LastIndex(name, "#request-")
	if marker < 0 {
		return name, "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(name[marker+9:])
	if err != nil || len(raw) == 0 || len(raw) > 256 {
		return "", "", errors.New("invalid detached request identity")
	}
	return name[:marker], string(raw), nil
}
