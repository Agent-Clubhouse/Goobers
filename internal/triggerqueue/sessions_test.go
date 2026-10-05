package triggerqueue

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/sessioning"
)

func sessionCommand(key string) SessionCommand {
	return SessionCommand{Gaggle: "gaggle", Actor: sessioning.Actor{Issuer: "https://issuer.example", Subject: "alice"}, RequestID: key, RequestDigest: "sha256:" + childDigest([]byte(key))}
}
func sessionProfile() sessioning.Profile {
	return sessioning.Profile{Goober: "planner", ConfigGeneration: "sha256:" + strings.Repeat("a", 64), GooberDigest: "sha256:" + strings.Repeat("b", 64)}
}
func createSessionTest(t *testing.T, s *Store, key string) sessioning.Session {
	t.Helper()
	r, err := s.CreateSession(t.Context(), sessionCommand(key), "Plan work", sessionProfile(), childTestTime)
	if err != nil {
		t.Fatal(err)
	}
	return r.Session
}
func submitSessionTest(t *testing.T, s *Store, id, key string) sessioning.Acceptance {
	t.Helper()
	r, err := s.SubmitSessionMessage(t.Context(), sessionCommand(key), id, key, []byte(`{"trusted":true}`), childTestTime)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSessionAtomicOrderingAcrossStoresAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	s := openTestStore(t, path)
	other := openTestStore(t, path)
	session := createSessionTest(t, s, "create")
	var wg sync.WaitGroup
	receipts := make(chan sessioning.Acceptance, 2)
	failures := make(chan error, 2)
	for i, store := range []*Store{s, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := sessionCommand(fmt.Sprintf("input-%d", i))
			c.Actor.Subject = fmt.Sprintf("human-%d", i)
			r, err := store.SubmitSessionMessage(context.Background(), c, session.ID, "question", []byte(`{}`), childTestTime)
			receipts <- r
			failures <- err
		}()
	}
	wg.Wait()
	close(receipts)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first, second sessioning.Acceptance
	for r := range receipts {
		if r.Message.Sequence == 1 {
			first = r
		} else {
			second = r
		}
	}
	if first.Message == nil || second.Message == nil || second.Message.Sequence != 2 {
		t.Fatal("non-atomic sequence", first, second)
	}
	if _, err := s.BeginSessionTurn(t.Context(), second.AcceptanceID, childTestTime); !errors.Is(err, ErrTransition) {
		t.Fatal("out of order dispatch", err)
	}
	if _, err := s.SessionInputs(t.Context(), first.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	turn, err := other.BeginSessionTurn(t.Context(), first.AcceptanceID, childTestTime)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Message.RunID != "" || turn.Record.RunID != "" {
		t.Fatal("reserved execution exposed")
	}
	if _, err = s.BeginSessionTurn(t.Context(), second.AcceptanceID, childTestTime); !errors.Is(err, ErrTransition) {
		t.Fatal("two active turns", err)
	}
	run := strings.TrimPrefix(first.AcceptanceID, "trigger-")
	if err = s.ObserveSessionRun(t.Context(), first.AcceptanceID, run, childTestTime); err != nil {
		t.Fatal(err)
	}
	completion := SessionCompletion{RunID: run, Outcome: "success", Text: "planned"}
	if err = s.CompleteSessionTurn(t.Context(), first.AcceptanceID, completion, childTestTime); err != nil {
		t.Fatal(err)
	}
	if err = other.CompleteSessionTurn(t.Context(), first.AcceptanceID, completion, childTestTime); err != nil {
		t.Fatal("lost response replay", err)
	}
	completion.Text = "different"
	if err = s.CompleteSessionTurn(t.Context(), first.AcceptanceID, completion, childTestTime); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, err = s.SessionInputs(t.Context(), second.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.BeginSessionTurn(t.Context(), second.AcceptanceID, childTestTime); err != nil {
		t.Fatal(err)
	}
	page, err := other.SessionMessages(t.Context(), "gaggle", session.ID, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.NextCursor != 2 || page.Items[0].Actor.Subject == page.Items[1].Actor.Subject {
		t.Fatal(page)
	}
	page, err = other.SessionMessages(t.Context(), "gaggle", session.ID, page.NextCursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Actor != nil || page.Items[0].ActorKind != "agent" || page.Items[0].RunID != run {
		t.Fatal(page)
	}
	if _, err = s.SessionMessages(t.Context(), "foreign", session.ID, 0, 2); err == nil {
		t.Fatal("cross gaggle read")
	}
}

func TestSessionCloseKeepsUncertainCustodyAndCancelsQueued(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	session := createSessionTest(t, s, "create")
	first := submitSessionTest(t, s, session.ID, "first")
	second := submitSessionTest(t, s, session.ID, "second")
	if _, err := s.SessionInputs(t.Context(), first.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginSessionTurn(t.Context(), first.AcceptanceID, childTestTime); err != nil {
		t.Fatal(err)
	}
	closed, err := s.CloseSession(t.Context(), sessionCommand("close"), session.ID, "done", childTestTime)
	if err != nil {
		t.Fatal(err)
	}
	if closed.Session.State != sessioning.CancelRequested || closed.Session.ActiveTurnID != first.Message.TurnID {
		t.Fatal(closed)
	}
	turn, err := s.SessionTurn(t.Context(), second.AcceptanceID)
	if err != nil || turn.State != "settled" || turn.Record.State != Rejected {
		t.Fatal(turn, err)
	}
	if _, err = s.SubmitSessionMessage(t.Context(), sessionCommand("late"), session.ID, "late", []byte(`{}`), childTestTime); !errors.Is(err, ErrSessionClosed) {
		t.Fatal(err)
	}
	if _, err = s.PruneSessions(t.Context(), childTestTime.Add(365*24*time.Hour), 100); err != nil {
		t.Fatal(err)
	}
	turn, err = s.SessionTurn(t.Context(), first.AcceptanceID)
	if err != nil || turn.Record.State != Dispatching {
		t.Fatal("uncertain custody expired", turn, err)
	}
	// A verified stop before initial journal publication may settle without a
	// run link; a timeout alone must never call this host evidence method.
	if err = s.CompleteSessionTurn(t.Context(), first.AcceptanceID, SessionCompletion{Outcome: "cancelled", Text: "Cancelled before start."}, childTestTime); err != nil {
		t.Fatal(err)
	}
	actual, err := s.Session(t.Context(), "gaggle", session.ID)
	if err != nil || actual.State != sessioning.Closed {
		t.Fatal(actual, err)
	}
}

func TestSessionReplayCapacityAndRetention(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	session := createSessionTest(t, s, "create")
	first := submitSessionTest(t, s, session.ID, "first")
	replay := submitSessionTest(t, s, session.ID, "first")
	if !replay.Duplicate || replay.AcceptanceID != first.AcceptanceID {
		t.Fatal(replay)
	}
	changed := sessionCommand("first")
	changed.RequestDigest = "sha256:" + strings.Repeat("e", 64)
	if _, err := s.SubmitSessionMessage(t.Context(), changed, session.ID, "different", []byte(`{}`), childTestTime); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	for i := 1; i < sessioning.MaxQueuedTurns; i++ {
		submitSessionTest(t, s, session.ID, fmt.Sprintf("input-%d", i))
	}
	if _, err := s.SubmitSessionMessage(t.Context(), sessionCommand("overflow"), session.ID, "overflow", []byte(`{}`), childTestTime); !errors.Is(err, ErrFull) {
		t.Fatal(err)
	}
	page, err := s.SessionMessages(t.Context(), "gaggle", session.ID, 0, 200)
	if err != nil || len(page.Items) != sessioning.MaxQueuedTurns {
		t.Fatal(len(page.Items), err)
	}
	if _, err = s.CloseSession(t.Context(), sessionCommand("close"), session.ID, "done", childTestTime); err != nil {
		t.Fatal(err)
	}
	later := childTestTime.Add(sessioning.Retention + time.Hour)
	if _, _, err = s.Accept(t.Context(), "ordinary", "operator", []byte(`{}`), later); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SessionTurn(t.Context(), first.AcceptanceID); err != nil {
		t.Fatal("ordinary queue expiry removed session evidence", err)
	}
	if count, err := s.PruneSessions(t.Context(), later, 3); err != nil || count > 3 {
		t.Fatal(count, err)
	}
	for range 30 {
		if _, err = s.PruneSessions(t.Context(), later, 3); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err = s.SessionReplay(t.Context(), sessionCommand("first"), "message", session.ID); !errors.Is(err, ErrSessionExpired) {
		t.Fatal("tombstone replay", err)
	}
	for range 30 {
		if _, err = s.PruneSessions(t.Context(), later.Add(sessioning.Retention+time.Hour), 3); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		if _, err = s.PruneSessions(t.Context(), later.Add(2*sessioning.Retention+2*time.Hour), 100); err != nil {
			t.Fatal(err)
		}
	}
	page2, err := s.Sessions(t.Context(), "gaggle", "", 100)
	if err != nil || len(page2.Items) != 0 {
		t.Fatal("retention did not converge", page2, err)
	}
}
