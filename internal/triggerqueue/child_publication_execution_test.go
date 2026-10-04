package triggerqueue

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestChildPublicationEffectFencesOriginalExecutionAfterRestart(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	c := acceptChildTest(t, s, childRequest("parent", "occurrence", "key"), childTestTime)
	original, err := s.PrepareChildPublication(t.Context(), c.Identity, "branch", []byte(`{"emitter":"original"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BeginChildPublicationEffect(t.Context(), original); err != nil {
		t.Fatal(err)
	}
	c = settleChildEpoch(t, s, c, childTestTime.Add(time.Second))
	e, _, err := s.BeginChildRestart(t.Context(), childRestartRequest(c, "human-epoch"), childTestTime.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BeginChildPublicationEffect(t.Context(), original); !errors.Is(err, ErrTransition) {
		t.Fatal("old execution began an effect", err)
	}
	if _, err = s.PrepareChildPublication(t.Context(), c.Identity, "pr", []byte(`{"emitter":"original"}`)); !errors.Is(err, ErrTransition) {
		t.Fatal("old execution admitted a new intent", err)
	}
	if err = s.BeginChildExecutionPublicationEffect(t.Context(), original, e.RunID); !errors.Is(err, ErrTransition) {
		t.Fatal("current epoch borrowed original intent", err)
	}
	if err = s.ConfirmChildPublication(t.Context(), original, []byte(`{"observed":"original effect"}`)); err != nil {
		t.Fatal("late exact observation was lost", err)
	}
	if _, err = s.PrepareChildExecutionPublication(t.Context(), c.Identity, e.RunID, "branch", []byte(`{"emitter":"epoch"}`)); !errors.Is(err, ErrConflict) {
		t.Fatal("epoch replaced original immutable intent", err)
	}
}

func TestChildPublicationFirstEffectUsesCurrentEpochAndCancelsAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	s := openTestStore(t, path)
	c, _ := failedRestartChild(t, s)
	e, _, err := s.BeginChildRestart(t.Context(), childRestartRequest(c, "human-epoch"), childTestTime.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	intent, err := s.PrepareChildExecutionPublication(t.Context(), c.Identity, e.RunID, "branch", []byte(`{"emitter":"epoch"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BeginChildExecutionPublicationEffect(t.Context(), intent, c.RunID); !errors.Is(err, ErrTransition) {
		t.Fatal("original execution borrowed epoch admission", err)
	}
	if err = s.BeginChildExecutionPublicationEffect(t.Context(), intent, e.RunID); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, path)
	if err = s.FenceChildParent(t.Context(), c.Identity.ChildParent, "human", childTestTime.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = s.BeginChildExecutionPublicationEffect(t.Context(), intent, e.RunID); !errors.Is(err, ErrParentCancelled) {
		t.Fatal("cancelled epoch began an effect", err)
	}
	if err = s.ConfirmChildPublication(t.Context(), intent, []byte(`{"observed":"epoch effect"}`)); err != nil {
		t.Fatal("cancelled epoch observation was lost", err)
	}
}
