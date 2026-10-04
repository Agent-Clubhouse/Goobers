package triggerqueue

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/sessioning"
)

func TestSessionInputIncludesSettledReplyBeforeQueuedHumanAndFreezes(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	session := createSessionTest(t, s, "create")
	first := submitSessionTest(t, s, session.ID, "human one")
	second := submitSessionTest(t, s, session.ID, "human two")
	if _, err := s.SessionInputs(t.Context(), second.AcceptanceID); !errors.Is(err, ErrTransition) {
		t.Fatal("froze context before earlier reply", err)
	}
	if _, err := s.SessionInputs(t.Context(), first.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginSessionTurn(t.Context(), first.AcceptanceID, childTestTime); err != nil {
		t.Fatal(err)
	}
	runID := strings.TrimPrefix(first.AcceptanceID, "trigger-")
	if err := s.ObserveSessionRun(t.Context(), first.AcceptanceID, runID, childTestTime); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteSessionTurn(t.Context(), first.AcceptanceID, SessionCompletion{RunID: runID, Outcome: "success", Text: "agent one"}, childTestTime); err != nil {
		t.Fatal(err)
	}
	inputs, err := s.SessionInputs(t.Context(), second.AcceptanceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs.Messages) != 3 || inputs.Messages[0].Text != "human one" || inputs.Messages[1].Text != "agent one" || inputs.Messages[2].Text != "human two" {
		t.Fatal(inputs.Messages)
	}
	if inputs.Messages[1].Sequence <= inputs.Messages[2].Sequence {
		t.Fatal("fixture missed delayed response ordering")
	}
	submitSessionTest(t, s, session.ID, "human three")
	replay, err := s.SessionInputs(t.Context(), second.AcceptanceID)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(inputs)
	b, _ := json.Marshal(replay)
	if string(a) != string(b) {
		t.Fatal("later human changed admitted context")
	}
	if _, err = s.db.Exec(`UPDATE interactive_turns SET authority='{"changed":true}' WHERE acceptance_id=?`, second.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SessionInputs(t.Context(), second.AcceptanceID); err == nil {
		t.Fatal("changed human authority accepted")
	}
}

func TestSessionInputAndPageBytesAreBounded(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	session := createSessionTest(t, s, "create")
	text := strings.Repeat("x", sessioning.MaxTextBytes)
	for i := range 4 {
		c := sessionCommand(string(rune('a' + i)))
		r, err := s.SubmitSessionMessage(t.Context(), c, session.ID, text, []byte(`{}`), childTestTime)
		if err != nil {
			t.Fatal(err)
		}
		inputs, err := s.SessionInputs(t.Context(), r.AcceptanceID)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(inputs)
		if len(raw) > sessioning.MaxContextBytes {
			t.Fatal("unbounded context")
		}
		if i > 1 && !inputs.Truncated {
			t.Fatal("missing explicit context truncation")
		}
		if _, err = s.BeginSessionTurn(t.Context(), r.AcceptanceID, childTestTime); err != nil {
			t.Fatal(err)
		}
		runID := strings.TrimPrefix(r.AcceptanceID, "trigger-")
		if err = s.ObserveSessionRun(t.Context(), r.AcceptanceID, runID, childTestTime); err != nil {
			t.Fatal(err)
		}
		if err = s.CompleteSessionTurn(t.Context(), r.AcceptanceID, SessionCompletion{RunID: runID, Outcome: "success", Text: text}, childTestTime); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 12 {
		if _, err := s.SubmitSessionMessage(t.Context(), sessionCommand("queued-"+string(rune('a'+i))), session.ID, text, []byte(`{}`), childTestTime); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.SessionMessages(t.Context(), "gaggle", session.ID, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	size := 0
	for _, m := range page.Items {
		size += len(m.Text)
	}
	if size > sessioning.MaxMessagePageBytes || page.NextCursor == 0 {
		t.Fatal("page byte bound missing", size, page.NextCursor)
	}
}
