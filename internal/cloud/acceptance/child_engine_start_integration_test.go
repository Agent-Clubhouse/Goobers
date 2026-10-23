//go:build cloudintegration

package acceptance

import (
	"errors"
	"testing"
	"time"

	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func engineStartFor(child triggerqueue.ChildRecord, run string) ChildEngineStart {
	return ChildEngineStart{ManifestDigest: childDigest([]byte("manifest")), ParentWorkflow: "parent-workflow", ParentWorkflowID: "parent-wf",
		ParentTemporalRunID: run, Deadline: child.AcceptedAt.Add(time.Hour)}
}

func TestChildEngineStartIsBoundOnceAndPinsItsDeadline(t *testing.T) {
	store, _ := childTestStore(t)
	child, _, err := store.AcceptChild(t.Context(), childRequest("parent", "stage", "call"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	want := engineStartFor(child, "run-1")
	got, err := store.BindChildEngineStart(t.Context(), child.Identity, want)
	if err != nil || got != want {
		t.Fatalf("bind = %+v %v", got, err)
	}
	moved := want
	moved.Deadline = want.Deadline.Add(time.Hour)
	if got, err := store.BindChildEngineStart(t.Context(), child.Identity, moved); err != nil || got != want {
		t.Fatalf("retry moved the pinned deadline: %+v %v", got, err)
	}
	if got, err := store.ChildEngineStart(t.Context(), child.Identity); err != nil || got != want {
		t.Fatalf("read = %+v %v", got, err)
	}
	for name, change := range map[string]func(*ChildEngineStart){
		"reset parent run": func(s *ChildEngineStart) { s.ParentTemporalRunID = "run-2" },
		"other parent":     func(s *ChildEngineStart) { s.ParentWorkflowID = "other-wf" },
		"different freeze": func(s *ChildEngineStart) { s.ManifestDigest = childDigest([]byte("other")) },
		"renamed workflow": func(s *ChildEngineStart) { s.ParentWorkflow = "renamed" },
	} {
		t.Run(name, func(t *testing.T) {
			other := want
			change(&other)
			if _, err := store.BindChildEngineStart(t.Context(), child.Identity, other); !errors.Is(err, ErrChildEngineStartOwned) {
				t.Fatalf("rebind = %v", err)
			}
		})
	}
}

func TestChildEngineStartRefusesUnadmittableLineage(t *testing.T) {
	store, _ := childTestStore(t)
	child, _, err := store.AcceptChild(t.Context(), childRequest("parent", "stage", "call"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FenceChildParent(t.Context(), child.Identity.ChildParent, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindChildEngineStart(t.Context(), child.Identity, engineStartFor(child, "run-1")); !errors.Is(err, triggerqueue.ErrTransition) {
		t.Fatalf("fenced family accepted a start intent: %v", err)
	}
	missing := child.Identity
	missing.InvocationKey = "absent"
	if _, err := store.BindChildEngineStart(t.Context(), missing, engineStartFor(child, "run-1")); err == nil {
		t.Fatal("start intent bound to an unaccepted child")
	}
}

func TestChildEngineStartSurvivesLeaseTakeover(t *testing.T) {
	old, pool := childTestStore(t)
	child, _, err := old.AcceptChild(t.Context(), childRequest("parent", "stage", "call"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	want := engineStartFor(child, "run-1")
	if _, err := old.BindChildEngineStart(t.Context(), child.Identity, want); err != nil {
		t.Fatal(err)
	}
	predecessor := old.handle.Epoch
	if err := old.manager.Release(t.Context(), old.handle); err != nil {
		t.Fatal(err)
	}
	current := successorStore(t, pool)
	current.BindChildBlobs(old.childBlobs)
	// Custody moves only through adoption; the successor cannot write first.
	if _, err := current.BindChildEngineStart(t.Context(), child.Identity, want); !errors.Is(err, triggerqueue.ErrTransition) {
		t.Fatalf("bind before adoption = %v", err)
	}
	if _, err := current.AdoptChildren(t.Context(), predecessor); err != nil {
		t.Fatal(err)
	}
	if got, err := current.BindChildEngineStart(t.Context(), child.Identity, want); err != nil || got != want {
		t.Fatalf("successor retry = %+v %v", got, err)
	}
	var epoch int64
	if err := pool.QueryRow(t.Context(), `SELECT owner_epoch FROM control.child_engine_start WHERE instance_id=$1 AND child_id=$2`, current.instanceID, child.ChildID).Scan(&epoch); err != nil || epoch != current.handle.Epoch {
		t.Fatalf("start intent epoch=%d err=%v, want %d", epoch, err, current.handle.Epoch)
	}
	reset := want
	reset.ParentTemporalRunID = "run-after-reset"
	if _, err := current.BindChildEngineStart(t.Context(), child.Identity, reset); !errors.Is(err, ErrChildEngineStartOwned) {
		t.Fatalf("successor re-bound to a reset parent: %v", err)
	}
	if _, err := old.BindChildEngineStart(t.Context(), child.Identity, want); !errors.Is(err, cloudlease.ErrStale) && !errors.Is(err, triggerqueue.ErrTransition) {
		t.Fatalf("stale predecessor wrote a start intent: %v", err)
	}
}
