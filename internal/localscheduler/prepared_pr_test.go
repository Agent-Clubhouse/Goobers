package localscheduler

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
)

type preparedPRRequestStarter struct{ requests chan StartRequest }

func (s *preparedPRRequestStarter) Start(_ context.Context, req StartRequest) (StartResult, error) {
	s.requests <- req
	return StartResult{Phase: journal.PhaseCompleted}, nil
}

func TestPreparedOrdinaryPRUsesAdmissionValidationAndReservedIdentity(t *testing.T) {
	log, _, err := journal.OpenInstanceLog(filepath.Join(t.TempDir(), "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	starter := &preparedPRRequestStarter{requests: make(chan StartRequest, 1)}
	entry := WorkflowEntry{Workflow: "review", Gaggle: "own", Signals: []string{webhookhttp.SignalName("pull_request")}, Starter: starter}
	admission, cancel := context.WithCancel(t.Context())
	defer cancel()
	validated := false
	scheduler := New([]WorkflowEntry{entry}, log, WithTargetedPRValidator(func(ctx context.Context, got WorkflowEntry, number int) error {
		validated = true
		if ctx != admission || got.Workflow != entry.Workflow || number != 42 {
			t.Fatal("validation changed scope")
		}
		return nil
	}))
	runID := "0123456789abcdef0123456789abcdef"
	if got, err := scheduler.TriggerPreparedOrdinary(admission, t.Context(), entry, runID, PreparedTriggerOptions{PullRequest: 42}, time.Now()); err != nil || got != runID {
		t.Fatal(got, err)
	}
	cancel()
	scheduler.Wait()
	request := <-starter.requests
	if !validated || request.RunID != runID || !request.RequireDurableJournal || request.Trigger.Ref != webhookhttp.TriggerRef(webhookhttp.Delivery{Event: "pull_request", PullNumber: 42}) {
		t.Fatal(request)
	}
	// Removing the current subscription must block an older accepted target.
	current := entry
	current.Signals = nil
	if err := scheduler.Reload([]WorkflowEntry{current}, nil, time.Now(), "old", "new"); err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.TriggerPreparedOrdinary(t.Context(), t.Context(), entry, "11111111111111111111111111111111", PreparedTriggerOptions{PullRequest: 42}, time.Now()); err == nil {
		t.Fatal("removed subscription admitted")
	}
}

func TestPreparedOrdinaryPRValidationErrorDoesNotStart(t *testing.T) {
	log, _, err := journal.OpenInstanceLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	starter := &preparedPRRequestStarter{requests: make(chan StartRequest, 1)}
	entry := WorkflowEntry{Workflow: "review", Gaggle: "own", Signals: []string{webhookhttp.SignalName("pull_request")}, Starter: starter}
	refused := errors.New("PR repository mismatch")
	s := New([]WorkflowEntry{entry}, log, WithTargetedPRValidator(func(context.Context, WorkflowEntry, int) error { return refused }))
	if _, err := s.TriggerPreparedOrdinary(t.Context(), t.Context(), entry, "0123456789abcdef0123456789abcdef", PreparedTriggerOptions{PullRequest: 42}, time.Now()); !errors.Is(err, refused) {
		t.Fatal(err)
	}
	select {
	case req := <-starter.requests:
		t.Fatal(req)
	default:
	}
}
