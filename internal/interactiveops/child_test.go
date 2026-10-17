package interactiveops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/platform/lock"
)

func TestReviewChildCapacityWaitHonorsContextWithoutStoppingParent(t *testing.T) {
	svc, parent := fixture(t, os.Args[0])
	svc.opts.MaxActivePerOwner = 1
	if _, err := svc.Snapshot(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	held, err := lock.TryAcquire(filepath.Join(svc.scopeDir(parent), "worker.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	child := Scope{ActorID: parent.ActorID, SessionID: parent.SessionID + "--capacity"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := svc.RunReviewChild(ctx, child, "bounded-wait", "Only run the regression tests", func(event ReviewChildEvent) error {
		if event.Action != nil && event.Action.Waiting == "capacity" {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || result.OperationStatus != "prepared" || result.Waiting != "capacity" {
		t.Fatalf("capacity should remain undispatched: %+v %v", result, err)
	}
	if _, err = os.Stat(filepath.Join(svc.scopeDir(parent), "stopped")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("child stop fenced its parent")
	}
}
