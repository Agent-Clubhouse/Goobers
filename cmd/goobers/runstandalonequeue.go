package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

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
		return "", err
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
