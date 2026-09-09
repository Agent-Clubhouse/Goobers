package main

import (
	"errors"
	"net/http"
	"testing"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func TestPersistentCancelReplaysWithoutExecutingAfterRestart(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	s, err := newPersistentDaemonCancelService(layout, newDaemonRunnerRegistry(), log)
	if err != nil {
		t.Fatal(err)
	}
	input := httpapi.CancelRunRequest{RunID: "run-1", Actor: "operator", IdempotencyKey: "key"}
	want, err := s.Cancel(t.Context(), input)
	if err != nil || want.Code != httpapi.CancelCodeNotRunning {
		t.Fatalf("result=%+v err=%v", want, err)
	}
	if err := s.receipts.Close(); err != nil {
		t.Fatal(err)
	}
	// A duplicate must return before resolving any runner. A nil registry
	// makes accidental re-execution fail loudly.
	s, err = newPersistentDaemonCancelService(layout, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.receipts.Close() }()
	got, err := s.Cancel(t.Context(), input)
	if err != nil || got != want {
		t.Fatalf("got=%+v want=%+v err=%v", got, want, err)
	}
	events, err := journal.ReadInstanceLog(log.Dir())
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	input.RunID = "another-run"
	_, err = s.Cancel(t.Context(), input)
	var refusal *httpapi.InterventionError
	if !errors.As(err, &refusal) || refusal.Code != "cancel_request_conflict" || refusal.Status != http.StatusConflict {
		t.Fatalf("conflict=%v", err)
	}
}

func TestPersistentCancelAuditFailureRemainsUncertain(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := newPersistentDaemonCancelService(layout, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.receipts.Close() }()
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	input := httpapi.CancelRunRequest{RunID: "run-1", Actor: "operator", IdempotencyKey: "key"}
	if _, err := s.Cancel(t.Context(), input); !errors.Is(err, journal.ErrClosed) {
		t.Fatalf("audit error=%v", err)
	}
	_, err = s.Cancel(t.Context(), input)
	var refusal *httpapi.InterventionError
	if !errors.As(err, &refusal) || refusal.Code != "cancel_outcome_unknown" {
		t.Fatalf("uncertain=%v", err)
	}
}
