package main

import (
	"context"
	"errors"
	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/claimsclient"
	"strings"
	"testing"
	"time"
)

func TestNativeVerdictPublicationCustodySerializesFileAndPlane(t *testing.T) {
	for _, modes := range []string{"file/file", "plane/plane", "file/plane", "plane/file"} {
		t.Run(modes, func(t *testing.T) {
			plane := newClaimsPlane(t)
			local, err := fileClaimLedger(plane.layout)
			if err != nil {
				t.Fatal(err)
			}
			first := claimsclient.MergeLock{Key: claimsclient.MergeLockKey("test", "github", "publication/github/acme", "repo"), RunID: "first", Workflow: "merge-review"}
			second := first
			second.RunID = "second"
			clients := []claimsclient.Ledger{local, local}
			for i, mode := range strings.Split(modes, "/") {
				run := []string{"first", "second"}[i]
				token := plane.admitRun(t, "test", run)
				if mode == "plane" {
					clients[i] = plane.client(t, run, token)
				}
			}
			entered, release := make(chan struct{}), make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- withPublicationClaim(t.Context(), clients[0], first, time.Second, 5*time.Second, 10*time.Second, func(context.Context) error {
					close(entered)
					<-release
					return nil
				})
			}()
			<-entered
			err = withPublicationClaim(t.Context(), clients[1], second, 80*time.Millisecond, time.Second, 10*time.Second, func(context.Context) error { return errors.New("entered occupied custody") })
			close(release)
			if heldErr := <-done; heldErr != nil {
				t.Fatal(heldErr)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("contending publisher entered landing custody: %v", err)
			}
			enteredAfter := false
			err = withPublicationClaim(t.Context(), clients[1], second, time.Second, time.Second, 10*time.Second, func(context.Context) error { enteredAfter = true; return nil })
			if err != nil || !enteredAfter {
				t.Fatalf("custody not released: %v", err)
			}
		})
	}
}

func TestNativeVerdictPublicationCustodyRetainsLeaseOnUncertainCompletion(t *testing.T) {
	plane := newClaimsPlane(t)
	ledger, err := fileClaimLedger(plane.layout)
	if err != nil {
		t.Fatal(err)
	}
	lock := claimsclient.MergeLock{Key: claimsclient.MergeLockKey("test", "github", "publication/github/acme", "repo"), RunID: "first", Workflow: "merge-review"}
	err = withPublicationClaim(t.Context(), ledger, lock, time.Second, 20*time.Millisecond, time.Second, func(ctx context.Context) error { <-ctx.Done(); return nil })
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "outcome uncertain") {
		t.Fatalf("uncertain completion reported as success: %v", err)
	}
	retryErr := withPublicationClaim(t.Context(), ledger, lock, 40*time.Millisecond, 100*time.Millisecond, time.Second, func(context.Context) error {
		t.Error("same-run retry bypassed the retained quarantine")
		return nil
	})
	if !errors.Is(retryErr, context.DeadlineExceeded) {
		t.Fatalf("same-run retry did not wait: %v", retryErr)
	}
	ok, holder, err := ledger.ClaimScoped(t.Context(), lock.Key, "second", "merge-review", time.Second)
	if err != nil || ok || holder != "first" {
		t.Fatalf("uncertain landing released its quarantine: ok=%t holder=%q err=%v", ok, holder, err)
	}
}

func TestNativeVerdictPublicationCustodyDetectsLossAfterAction(t *testing.T) {
	plane := newClaimsPlane(t)
	ledger, err := fileClaimLedger(plane.layout)
	if err != nil {
		t.Fatal(err)
	}
	lock := claimsclient.MergeLock{Key: claimsclient.MergeLockKey("test", "github", "publication/github/acme", "repo"), RunID: "first", Workflow: "merge-review"}
	err = withPublicationClaim(t.Context(), ledger, lock, time.Second, time.Second, 5*time.Second, func(ctx context.Context) error {
		_, e := ledger.ReleaseAllForRun(ctx, lock.RunID, claimrelease.Completed.Because("test", ""))
		return e
	})
	if err == nil || !strings.Contains(err.Error(), "custody lost") {
		t.Fatalf("lost custody reported as success: %v", err)
	}
}
