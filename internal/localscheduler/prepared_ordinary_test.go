package localscheduler

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

type preparedRequestStarter struct{ requests chan StartRequest }

func (s *preparedRequestStarter) Start(_ context.Context, req StartRequest) (StartResult, error) {
	s.requests <- req
	return StartResult{Phase: journal.PhaseCompleted}, nil
}

func TestPreparedOrdinaryUsesCapturedStarterAndReservedIdentity(t *testing.T) {
	log, _, err := journal.OpenInstanceLog(filepath.Join(t.TempDir(), "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	starter := &preparedRequestStarter{requests: make(chan StartRequest, 1)}
	entry := WorkflowEntry{Workflow: "review", Gaggle: "own", Starter: starter}
	replacement := &preparedRequestStarter{requests: make(chan StartRequest, 1)}
	current := entry
	current.Starter = replacement
	admission, cancel := context.WithCancel(t.Context())
	defer cancel()
	scheduler := New([]WorkflowEntry{current}, log)
	runID := "0123456789abcdef0123456789abcdef"
	if got, err := scheduler.TriggerPreparedOrdinary(admission, t.Context(), entry, runID, PreparedTriggerOptions{SourceRun: "source-run"}, time.Now()); err != nil || got != runID {
		t.Fatal(got, err)
	}
	cancel()
	scheduler.Wait()
	request := <-starter.requests
	if request.RunID != runID || !request.RequireDurableJournal || request.Trigger != (journal.Trigger{Kind: journal.TriggerSignal, Ref: "priority-re-tick:source-run"}) {
		t.Fatal(request)
	}
	select {
	case req := <-replacement.requests:
		t.Fatal("used current definition", req)
	default:
	}
	// Current disable must block even a forced older accepted target.
	current.DisabledReason = "disabled after acceptance"
	if err := scheduler.Reload([]WorkflowEntry{current}, nil, time.Now(), "old", "new"); err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.TriggerPreparedOrdinary(t.Context(), t.Context(), entry, "11111111111111111111111111111111", PreparedTriggerOptions{Force: true}, time.Now()); err == nil {
		t.Fatal("force bypassed current disable")
	}
}

func TestPreparedOrdinaryRepositoryChangeDoesNotStart(t *testing.T) {
	log, _, err := journal.OpenInstanceLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	starter := &preparedRequestStarter{requests: make(chan StartRequest, 1)}
	entry := WorkflowEntry{Workflow: "review", Gaggle: "own", Starter: starter}
	current := entry
	current.RepoRef.Name = "different-repository"
	s := New([]WorkflowEntry{current}, log)
	if _, err := s.TriggerPreparedOrdinary(t.Context(), t.Context(), entry, "0123456789abcdef0123456789abcdef", PreparedTriggerOptions{}, time.Now()); err == nil {
		t.Fatal("repository change admitted")
	}
	select {
	case req := <-starter.requests:
		t.Fatal(req)
	default:
	}
}
