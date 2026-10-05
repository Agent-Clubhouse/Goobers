package interactivesession

import (
	"context"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/startcontrol"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func requestTurnCancellation(t *testing.T, s *Service, turn triggerqueue.SessionTurn) {
	t.Helper()
	meta, err := startcontrol.Describe(t.Context(), s.Queue, turn.Record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Queue.PinStartControl(t.Context(), turn.Record.ID, meta.Scope); err != nil {
		t.Fatal(err)
	}
	command := triggerqueue.StartCancellation{RequestID: "stop-turn", Actor: "alice", Reason: "Change of plan", Authority: []byte(`{"issuer":"https://identity.example","subject":"alice"}`)}
	if _, _, err = s.Queue.RequestStartCancellation(t.Context(), turn.Session.Gaggle, turn.Record.ID, command, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestSessionQueueCancellationJoinsExactTurnAndKeepsConversation(t *testing.T) {
	s, _, _ := serviceFixture(t)
	s.Now = time.Now
	h := installTestRuntime(t, s)
	h.joined = false
	accepted, turn := queuedTurn(t, s)
	if err := s.Dispatch(t.Context(), t.Context(), turn.Record); err != nil {
		t.Fatal(err)
	}
	waitSessionStart(t, h)
	next, err := s.SubmitMessage(t.Context(), sessionPrincipal("bob"), "gaggle", accepted.Session.ID, sessioning.MessageRequest{RequestID: "next", Text: "Keep this later turn"})
	if err != nil {
		t.Fatal(err)
	}
	requestTurnCancellation(t, s, turn)
	observed, err := s.CancelQueuedTurn(t.Context(), "gaggle", turn.Record.ID)
	if err != nil || observed.State != startcontrol.CancellationRequested {
		t.Fatal(observed, err)
	}
	s.Wait()
	current, err := s.Queue.SessionTurn(t.Context(), turn.Record.ID)
	if err != nil || current.State != "running" || current.Session.ActiveTurnID != turn.ID || h.released != 0 {
		t.Fatal("unknown writer released custody", current, h.released, err)
	}
	h.mu.Lock()
	h.joined = true
	h.mu.Unlock()
	observed, err = s.CancelQueuedTurn(t.Context(), "gaggle", turn.Record.ID)
	if err != nil || observed.State != startcontrol.CancellationConfirmed || h.released != 1 {
		t.Fatal(observed, h.released, err)
	}
	current, err = s.Queue.SessionTurn(t.Context(), turn.Record.ID)
	if err != nil || current.State != "settled" || current.Outcome != "cancelled" || current.Session.State != sessioning.Queued {
		t.Fatal("turn cancellation closed shared conversation", current, err)
	}
	nextTurn, err := s.Queue.SessionTurn(t.Context(), next.AcceptanceID)
	if err != nil || nextTurn.State != "queued" {
		t.Fatal(nextTurn, err)
	}
	if err = s.Dispatch(t.Context(), t.Context(), nextTurn.Record); err != nil {
		t.Fatal(err)
	}
	waitSessionStart(t, h)
	close(h.finish)
	s.Wait()
	if h.reserved != 2 || h.released != 2 {
		t.Fatal(h.reserved, h.released)
	}
}

func TestSessionQueueCancellationBusyDoesNotWaitOrInventStop(t *testing.T) {
	s, _, _ := serviceFixture(t)
	s.Now = time.Now
	h := installTestRuntime(t, s)
	_, turn := queuedTurn(t, s)
	if err := s.Dispatch(t.Context(), t.Context(), turn.Record); err != nil {
		t.Fatal(err)
	}
	waitSessionStart(t, h)
	requestTurnCancellation(t, s, turn)
	s.execution.mu.Lock()
	observed, err := s.CancelQueuedTurn(t.Context(), "gaggle", turn.Record.ID)
	s.execution.mu.Unlock()
	if err != nil || observed.State != startcontrol.CancellationRequested {
		t.Fatal(observed, err)
	}
	close(h.finish)
	s.Wait()
	observed, err = s.CancelQueuedTurn(t.Context(), "gaggle", turn.Record.ID)
	if err != nil || observed.State != startcontrol.CancellationAlreadyTerminal {
		t.Fatal("natural completion was claimed as a stop", observed, err)
	}
}

func TestSessionQueueCancellationBeforePublicationRequiresJoinedOwnerAndAbsence(t *testing.T) {
	s, _, _ := serviceFixture(t)
	s.Now = time.Now
	h := installTestRuntime(t, s)
	entered := make(chan struct{})
	original := s.Runtime.Build
	s.Runtime.Build = func(ctx context.Context, turn triggerqueue.SessionTurn, in sessioning.ExecutionInputs) (PreparedTurn, error) {
		prepared, err := original(ctx, turn, in)
		prepared.Run = func(ctx context.Context, _ *interactiveaccess.ExecutionLease, _ func() error) error {
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		}
		return prepared, err
	}
	_, turn := queuedTurn(t, s)
	if err := s.Dispatch(t.Context(), t.Context(), turn.Record); err != nil {
		t.Fatal(err)
	}
	<-entered
	requestTurnCancellation(t, s, turn)
	if _, err := s.CancelQueuedTurn(t.Context(), "gaggle", turn.Record.ID); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	observed, err := s.CancelQueuedTurn(t.Context(), "gaggle", turn.Record.ID)
	if err != nil || observed.State != startcontrol.CancellationConfirmed || h.released != 1 {
		t.Fatal(observed, h.released, err)
	}
	current, err := s.Queue.SessionTurn(t.Context(), turn.Record.ID)
	if err != nil || current.Record.RunID != "" || current.Record.State != triggerqueue.Rejected || current.Outcome != "cancelled" {
		t.Fatal("absence invented a run", current, err)
	}
}
