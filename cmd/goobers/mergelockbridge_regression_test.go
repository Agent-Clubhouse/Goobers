package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

func TestCrossRepoPlaneLeasesKeepFileMergeExcluded(t *testing.T) {
	plane := newClaimsPlane(t)
	fileClient, err := fileClaimLedger(plane.layout)
	if err != nil {
		t.Fatal(err)
	}
	a := plane.client(t, "run-a", plane.admitRun(t, "g", "run-a"))
	b := plane.client(t, "run-b", plane.admitRun(t, "g", "run-b"))
	lock := func(runID, repo string) claimsclient.MergeLock {
		return claimsclient.MergeLock{
			Key: claimsclient.MergeLockKey("g", "github", "acme", repo), RunID: runID, Workflow: "merge-review",
		}
	}
	aEntered, releaseA, aDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		aDone <- a.MergeLock(t.Context(), lock("run-a", "web"), func(context.Context) error {
			close(aEntered)
			<-releaseA
			return nil
		})
	}()
	<-aEntered
	if err := b.MergeLock(t.Context(), lock("run-b", "api"), func(context.Context) error { return nil }); err != nil {
		t.Fatalf("independent repository plane lease: %v", err)
	}

	fileEntered := make(chan struct{})
	fileDone := make(chan error, 1)
	go func() {
		fileDone <- fileClient.MergeLock(t.Context(), lock("run-file", "web"), func(context.Context) error {
			close(fileEntered)
			return nil
		})
	}()
	select {
	case <-fileEntered:
		close(releaseA)
		<-aDone
		t.Fatal("file merge entered while the first plane repository lease remained live")
	case <-time.After(300 * time.Millisecond):
	}
	close(releaseA)
	if err := <-aDone; err != nil {
		t.Fatal(err)
	}
	if err := <-fileDone; err != nil {
		t.Fatal(err)
	}
}

func TestDirectLedgerMergeReleaseFreesFileLock(t *testing.T) {
	plane := newClaimsPlane(t)
	fileClient, err := fileClaimLedger(plane.layout)
	if err != nil {
		t.Fatal(err)
	}
	client := plane.client(t, "run-p", plane.admitRun(t, "g", "run-p"))
	key := claimsclient.MergeLockKey("g", "github", "acme", "web")
	if ok, _, err := client.ClaimScoped(t.Context(), key, "run-p", "merge-review", time.Hour); err != nil || !ok {
		t.Fatalf("plane merge claim: ok=%v err=%v", ok, err)
	}
	if err := releaseClaimsForRun(plane.layout, nil, "run-p"); err != nil {
		t.Fatal(err)
	}
	if err := fileClient.MergeLock(t.Context(), claimsclient.MergeLock{Key: key, RunID: "run-file"}, func(context.Context) error {
		return nil
	}); err != nil {
		t.Fatalf("file merge after direct ledger release: %v", err)
	}
}

func TestMergeLockStartupRestoresLiveLedgerExclusion(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	key := claimsclient.MergeLockKey("g", "github", "acme", "web")
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.ClaimScoped(key, "run-p", "merge-review", time.Hour); err != nil || !ok {
		t.Fatalf("seed live merge lease: ok=%v err=%v", ok, err)
	}
	service := newDaemonClaimService(layout, nil, nil)
	if err := service.initializeMergeLock(); err != nil {
		t.Fatal(err)
	}
	defer service.close()
	held, err := platformlock.TryAcquire(filepath.Join(layout.SchedulerDir(), mergeLockFileName))
	if held != nil {
		_ = held.Release()
		t.Fatal("file merge lock acquired over restored plane lease")
	}
	if !errors.Is(err, platformlock.ErrHeld) {
		t.Fatalf("restored merge lock probe: %v", err)
	}
}

func TestMergeRenewUnderLocalHolderIsRefusedImmediately(t *testing.T) {
	service, layout := newClaimServiceFixture(t)
	key := claimsclient.MergeLockKey("g", "github", "acme", "web")
	fileClient, err := fileClaimLedger(layout)
	if err != nil {
		t.Fatal(err)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- fileClient.MergeLock(t.Context(), claimsclient.MergeLock{Key: key, RunID: "run-file"}, func(context.Context) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	started := time.Now()
	response, err := service.Renew(t.Context(), httpapi.ClaimRequest{
		Gaggle: "g", Provider: "github", ItemID: key.ExternalID, RunID: "run-plane",
		Workflow: "merge-review", LeaseSeconds: 60,
	})
	if err != nil || response.Ok || response.Holder != "local merge-pr" {
		t.Fatalf("renew under local holder = %+v, %v", response, err)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("renewal refusal blocked for %s", elapsed)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestMergeAcquireDoesNotStallUnrelatedReleaseAll(t *testing.T) {
	plane := newClaimsPlane(t)
	fileClient, err := fileClaimLedger(plane.layout)
	if err != nil {
		t.Fatal(err)
	}
	planeClient := plane.client(t, "run-p", plane.admitRun(t, "g", "run-p"))
	other := plane.client(t, "run-x", plane.admitRun(t, "g", "run-x"))
	key := claimsclient.MergeLockKey("g", "github", "acme", "web")
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- fileClient.MergeLock(t.Context(), claimsclient.MergeLock{Key: key, RunID: "run-file"}, func(context.Context) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	waitCtx, cancel := context.WithCancel(t.Context())
	planeDone := make(chan error, 1)
	go func() {
		planeDone <- planeClient.MergeLock(waitCtx, claimsclient.MergeLock{
			Key: key, RunID: "run-p", Workflow: "merge-review",
		}, func(context.Context) error { return nil })
	}()
	time.Sleep(50 * time.Millisecond)
	started := time.Now()
	if _, err := other.ReleaseAllForRun(t.Context(), "run-x", claimrelease.Completed.Reason()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("unrelated release-all stalled for %s", elapsed)
	}
	cancel()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	<-planeDone
}
