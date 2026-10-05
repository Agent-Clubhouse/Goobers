package triggerqueue

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/sessioning"
)

func pinSessionControl(t *testing.T, s *Store, id string) StartControl {
	t.Helper()
	turn, err := s.SessionTurn(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	scope := controlScope(turn.Record, "session")
	scope.Gaggle, scope.Workflow, scope.Generation = turn.Session.Gaggle, "session/"+turn.Session.ID, turn.Session.ConfigGeneration
	c, err := s.PinStartControl(t.Context(), id, scope)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestControlledSessionSettlementKeepsOtherWriterAndFIFO(t *testing.T) {
	for _, disposition := range []string{"cancelled", "expired"} {
		t.Run(disposition, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "queue.db")
			s := openTestStore(t, path)
			session := createSessionTest(t, s, "create")
			first := submitSessionTest(t, s, session.ID, "first")
			second := submitSessionTest(t, s, session.ID, "second")
			third := submitSessionTest(t, s, session.ID, "third")
			if _, err := s.SessionInputs(t.Context(), first.AcceptanceID); err != nil {
				t.Fatal(err)
			}
			active, err := s.BeginSessionTurn(t.Context(), first.AcceptanceID, childTestTime)
			if err != nil {
				t.Fatal(err)
			}
			run := strings.TrimPrefix(first.AcceptanceID, "trigger-")
			if err = s.ObserveSessionRun(t.Context(), first.AcceptanceID, run, childTestTime); err != nil {
				t.Fatal(err)
			}
			c := pinSessionControl(t, s, second.AcceptanceID)
			if disposition == "cancelled" {
				c, _, err = s.RequestStartCancellation(t.Context(), c.Scope.Gaggle, c.Record.ID, controlCommand(), childTestTime.Add(time.Minute))
			} else {
				c, _, err = s.ExpireStartControl(t.Context(), c.Scope.Gaggle, c.Record.ID, c.Scope.Deadline)
			}
			if err != nil || c.Disposition != disposition || c.Record.State != Rejected || c.Record.RunID != "" {
				t.Fatal(c, err)
			}
			other := openTestStore(t, path)
			settled, err := other.SessionTurn(t.Context(), second.AcceptanceID)
			if err != nil || settled.State != "settled" || settled.Session.ActiveTurnID != active.ID || settled.Session.State != sessioning.Running {
				t.Fatal("cancelled later turn released active writer", settled, err)
			}
			if _, err = other.BeginSessionTurn(t.Context(), third.AcceptanceID, childTestTime.Add(2*time.Hour)); !errors.Is(err, ErrTransition) {
				t.Fatal("another writer started", err)
			}
			if err = other.CompleteSessionTurn(t.Context(), first.AcceptanceID, SessionCompletion{RunID: run, Outcome: "success", Text: "First turn finished."}, childTestTime.Add(2*time.Hour)); err != nil {
				t.Fatal(err)
			}
			inputs, err := other.SessionInputs(t.Context(), third.AcceptanceID)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, message := range inputs.Messages {
				if message.TurnID == settled.ID && message.ActorKind == "system" {
					found = true
					if message.RunID != "" || !strings.Contains(message.Text, disposition) {
						t.Fatal("invented execution or disposition", message)
					}
				}
			}
			if !found {
				t.Fatal("later conversation lost cancellation explanation")
			}
			if _, err = other.BeginSessionTurn(t.Context(), third.AcceptanceID, childTestTime.Add(2*time.Hour)); err != nil {
				t.Fatal("settled queued turn blocked FIFO", err)
			}
		})
	}
}

func TestControlledSessionCancellationDoesNotSettleAttemptedTurn(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	session := createSessionTest(t, s, "create")
	accepted := submitSessionTest(t, s, session.ID, "first")
	c := pinSessionControl(t, s, accepted.AcceptanceID)
	if _, err := s.SessionInputs(t.Context(), accepted.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	before, err := s.BeginSessionTurn(t.Context(), accepted.AcceptanceID, childTestTime)
	if err != nil {
		t.Fatal(err)
	}
	c, _, err = s.RequestStartCancellation(t.Context(), c.Scope.Gaggle, c.Record.ID, controlCommand(), childTestTime.Add(time.Minute))
	if err != nil || c.Cancellation == nil || c.Disposition != "" {
		t.Fatal(c, err)
	}
	after, err := s.SessionTurn(t.Context(), accepted.AcceptanceID)
	if err != nil || after.State != "dispatching" || after.Session.ActiveTurnID != before.ID || after.Record.State != Dispatching {
		t.Fatal("requested cancellation released uncertain writer", after, err)
	}
}

func TestControlledChildCancellationPreservesResultAndParentSlotCustody(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	req := childRequest("parent", "stage", "first")
	child := acceptChildTest(t, s, req, childTestTime)
	record, err := s.ChildStart(t.Context(), child.Identity)
	if err != nil {
		t.Fatal(err)
	}
	scope := controlScope(record, "child")
	scope.Gaggle = child.Identity.Gaggle
	c, err := s.PinStartControl(t.Context(), record.ID, scope)
	if err != nil {
		t.Fatal(err)
	}
	c, _, err = s.RequestStartCancellation(t.Context(), c.Scope.Gaggle, c.Record.ID, controlCommand(), childTestTime.Add(time.Minute))
	if err != nil || c.Disposition != "cancelled" || c.Record.State != Rejected {
		t.Fatal(c, err)
	}
	current, err := s.GetChild(t.Context(), child.Identity)
	if err != nil || current.State != ChildQueued || current.ResultRef != "" || !current.AcknowledgedAt.IsZero() {
		t.Fatal("queue invented a child terminal result", current, err)
	}
	if err = s.BeginDispatchAt(t.Context(), record.ID, childTestTime.Add(2*time.Minute)); !errors.Is(err, ErrTransition) {
		t.Fatal("cancelled child started", err)
	}
	req.Identity.InvocationKey = "next"
	if _, _, err = s.AcceptChild(t.Context(), req, childTestTime.Add(2*time.Minute)); !errors.Is(err, ErrChildSlotOccupied) {
		t.Fatal("unobserved result released parent slot", err)
	}
	at, _, err := s.ChildRejection(t.Context(), child.Identity)
	if err != nil || !at.Equal(c.DisposedAt) {
		t.Fatal("terminal observer lost actual disposition time", at, err)
	}
}
