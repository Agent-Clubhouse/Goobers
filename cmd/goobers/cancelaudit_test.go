package main

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
)

func TestDaemonCancelJournalsAttributedAttempt(t *testing.T) {
	log, _, err := journal.OpenInstanceLog(filepath.Join(t.TempDir(), "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	service := newDaemonCancelService(newDaemonRunnerRegistry())
	service.auditLog = log
	input := httpapi.CancelRunRequest{RunID: "run-1", Actor: "operator", IdempotencyKey: "delivery-1", Workflow: "impl", Gaggle: "own"}
	result, err := service.Cancel(t.Context(), input)
	if err != nil || result.Code != httpapi.CancelCodeNotRunning {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	events, err := journal.ReadInstanceLog(log.Dir())
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%+v error=%v", events, err)
	}
	event := events[0]
	if event.Type != journal.EventRunnerAnnotation || event.RunID != input.RunID || event.Actor != input.Actor || event.Workflow != input.Workflow || event.Gaggle != input.Gaggle {
		t.Fatalf("attribution=%+v", event)
	}
	if event.Runner["idempotencyKey"] != input.IdempotencyKey || event.Runner["note"] != "run.cancel.requested" {
		t.Fatalf("metadata=%+v", event.Runner)
	}
}

func TestDaemonCancelAuditFailurePreventsExecution(t *testing.T) {
	log, _, err := journal.OpenInstanceLog(filepath.Join(t.TempDir(), "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	// Deliberately omit the runner registry: reaching execution would panic.
	service := newDaemonCancelService(nil)
	service.auditLog = log
	result, err := service.Cancel(t.Context(), httpapi.CancelRunRequest{RunID: "run-1", Actor: "operator", IdempotencyKey: "delivery-1"})
	if !errors.Is(err, journal.ErrClosed) || result != (httpapi.CancelRunResult{}) {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}
