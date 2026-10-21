package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	platformlock "github.com/goobers/goobers/internal/platform/lock"
	"github.com/goobers/goobers/internal/sharedclaim"
	"github.com/goobers/goobers/providers"
)

func TestCrossRepoPlaneLeasesKeepFileMergeExcluded(t *testing.T) {
	plane := newClaimsPlane(t)
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

	mergePath := filepath.Join(plane.layout.SchedulerDir(), mergeLockFileName)
	if local, err := platformlock.TryAcquire(mergePath); !errors.Is(err, platformlock.ErrHeld) {
		if local != nil {
			_ = local.Release()
		}
		t.Fatalf("file merge lock was not excluded while run-a remained live: %v", err)
	}
	close(releaseA)
	if err := <-aDone; err != nil {
		t.Fatal(err)
	}
	local, err := platformlock.TryAcquire(mergePath)
	if err != nil {
		t.Fatal(err)
	}
	_ = local.Release()
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
	response, err := service.Renew(t.Context(), httpapi.ClaimRequest{
		Gaggle: "g", Provider: "github", ItemID: key.ExternalID, RunID: "run-plane",
		Workflow: "merge-review", LeaseSeconds: 60,
	})
	if err != nil || response.Ok || response.Holder != "local merge-pr" {
		t.Fatalf("renew under local holder = %+v, %v", response, err)
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
	response, err := plane.service.Acquire(t.Context(), httpapi.ClaimRequest{
		Gaggle: "g", Provider: "github", ItemID: key.ExternalID, RunID: "run-p",
		Workflow: "merge-review", LeaseSeconds: 60,
	})
	if err != nil || response.Ok || response.Holder != "local merge-pr" {
		t.Fatalf("plane acquire under local holder = %+v, %v", response, err)
	}
	if _, err := plane.service.releaseAllForRun(t.Context(), httpapi.ClaimRequest{
		Gaggle: "g", Provider: "github", RunID: "run-x", Reason: claimrelease.Completed.Reason(),
	}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestStaleRenewCannotDropNewerMergeLeaseFlock(t *testing.T) {
	service, layout := newClaimServiceFixture(t)
	mergePath := filepath.Join(layout.SchedulerDir(), mergeLockFileName)
	request := func(runID, repo string) httpapi.ClaimRequest {
		key := claimsclient.MergeLockKey("g", "github", "acme", repo)
		return httpapi.ClaimRequest{
			Gaggle: "g", Provider: "github", ItemID: key.ExternalID, RunID: runID,
			Workflow: "merge-review", LeaseSeconds: 600,
		}
	}
	for range 500 {
		var wg sync.WaitGroup
		wg.Go(func() {
			_, _ = service.Renew(t.Context(), request("run-stale", "stale"))
		})
		wg.Go(func() {
			response, err := service.Acquire(t.Context(), request("run-b", "api"))
			if err != nil || !response.Ok {
				t.Errorf("grant newer lease: %+v %v", response, err)
			}
		})
		wg.Wait()
		if local, err := platformlock.TryAcquire(mergePath); !errors.Is(err, platformlock.ErrHeld) {
			if local != nil {
				_ = local.Release()
			}
			t.Fatalf("merge lock free after concurrent stale renew and grant: %v", err)
		}
		if _, err := service.Release(t.Context(), request("run-b", "api")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSharedMergeLeaseRenewsThroughSharedCoordinator(t *testing.T) {
	layout, _ := newPinnedClaimResolverRun(t, "shared")
	store := &pinnedClaimTestStore{}
	service := newDaemonClaimService(layout, nil, nil)
	t.Cleanup(service.close)
	service.shared = pinnedSharedClaimResolver{
		layout: layout,
		store: func(context.Context, providers.RepositoryRef) (sharedclaim.Store, error) {
			return store, nil
		},
	}
	if err := service.initializeMergeLock(); err != nil {
		t.Fatal(err)
	}
	key := claimsclient.MergeLockKey("example", "github", "acme", "web")
	request := httpapi.ClaimRequest{
		Gaggle: "example", Provider: "github", ItemID: key.ExternalID,
		RunID: "shared-run", Workflow: "claim", LeaseSeconds: 60,
	}
	acquired, err := service.Acquire(t.Context(), request)
	if err != nil || !acquired.Ok {
		t.Fatalf("shared merge acquire: %+v %v", acquired, err)
	}
	renewed, err := service.Renew(t.Context(), request)
	if err != nil || !renewed.Ok || store.writes != 2 {
		t.Fatalf("shared merge renew: %+v %v writes=%d", renewed, err, store.writes)
	}
}

func TestInterventionReclaimSkipsReleasedMergeLease(t *testing.T) {
	service, layout := newClaimServiceFixture(t)
	key := claimsclient.MergeLockKey("g", "github", "acme", "web")
	request := httpapi.ClaimRequest{
		Gaggle: "g", Provider: "github", ItemID: key.ExternalID,
		RunID: "run-p", Workflow: "merge-review", LeaseSeconds: 600,
	}
	if response, err := service.Acquire(t.Context(), request); err != nil || !response.Ok {
		t.Fatalf("acquire merge lease: %+v %v", response, err)
	}
	if _, err := service.Release(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	claims := interventionClaims{layout: layout}
	history, err := claims.History("run-p", "github")
	if err != nil {
		t.Fatal(err)
	}
	if ok, holder, err := claims.Reclaim(t.Context(), history, "g", "run-p", "merge-review"); err != nil || !ok {
		t.Fatalf("reclaim non-resumable history: ok=%v holder=%q err=%v", ok, holder, err)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range ledger.ForRunAll("run-p") {
		if strings.HasPrefix(entry.ExternalID, claimsclient.MergeLockItemPrefix) {
			t.Fatalf("intervention resurrected merge lease: %+v", entry)
		}
	}
}

func TestSweepRefreshReleasesFlockAfterForeignLedgerMutation(t *testing.T) {
	service, layout := newClaimServiceFixture(t)
	request := httpapi.ClaimRequest{
		Gaggle: "g", Provider: "github",
		ItemID: claimsclient.MergeLockKey("g", "github", "acme", "web").ExternalID,
		RunID:  "run-p", Workflow: "merge-review", LeaseSeconds: 60,
	}
	if response, err := service.Acquire(t.Context(), request); err != nil || !response.Ok {
		t.Fatalf("acquire merge lease: %+v %v", response, err)
	}
	alias := filepath.Join(t.TempDir(), "scheduler")
	if err := os.Symlink(layout.SchedulerDir(), alias); err != nil {
		t.Fatal(err)
	}
	if err := withClaimLock(filepath.Join(layout.SchedulerDir(), claimLockFileName), "foreign-release", func() error {
		ledger, err := localscheduler.OpenClaimLedger(filepath.Join(alias, claimLedgerFileName))
		if err != nil {
			return err
		}
		_, err = ledger.RecoverExpired(time.Now().Add(time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.recover(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	mergePath := filepath.Join(layout.SchedulerDir(), mergeLockFileName)
	local, err := platformlock.TryAcquire(mergePath)
	if err != nil {
		t.Fatalf("sweep did not refresh foreign release: %v", err)
	}
	_ = local.Release()
}

func TestLegacyPublicationClaimDoesNotCountAsMergeLease(t *testing.T) {
	entry := localscheduler.ClaimEntry{
		ExternalID: "merge-lock/publication/github/acme/web",
		ExpiresAt:  time.Now().Add(time.Hour),
	}
	if liveMergeLeaseCount([]localscheduler.ClaimEntry{entry}, time.Now()) != 0 {
		t.Fatal("legacy publication custody counted as an active merge lease")
	}
	if isMergeLockRequest(httpapi.ClaimRequest{ItemID: entry.ExternalID}) {
		t.Fatal("legacy publication custody routed through merge exclusion")
	}
}

func TestFileMergeWaitsForPlaneLeaseAfterDaemonCrash(t *testing.T) {
	plane := newClaimsPlane(t)
	request := httpapi.ClaimRequest{
		Gaggle: "g", Provider: "github",
		ItemID: claimsclient.MergeLockKey("g", "github", "acme", "web").ExternalID,
		RunID:  "run-plane", Workflow: "merge-review", LeaseSeconds: 600,
	}
	if response, err := plane.service.Acquire(t.Context(), request); err != nil || !response.Ok {
		t.Fatalf("plane merge acquire: %+v %v", response, err)
	}
	plane.service.close()

	claimsLockPath := filepath.Join(plane.layout.SchedulerDir(), claimLockFileName)
	mergeLockPath := filepath.Join(plane.layout.SchedulerDir(), mergeLockFileName)
	checked := make(chan struct{})
	var checkedOnce sync.Once
	local, err := claimsclient.NewFile(claimsclient.FileConfig{
		LedgerPath: filepath.Join(plane.layout.SchedulerDir(), claimLedgerFileName),
		Lock: func(operation string, fn func() error) error {
			checkedOnce.Do(func() { close(checked) })
			return withClaimLock(claimsLockPath, operation, fn)
		},
		MergeLock: func(fn func() error) error {
			return withFileLock(mergeLockPath, fn)
		},
		MergeLockPoll: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- local.MergeLock(t.Context(), claimsclient.MergeLock{
			Key: requestClaimKey(request), RunID: "run-file", Workflow: "merge-review",
		}, func(context.Context) error {
			close(entered)
			return nil
		})
	}()
	<-checked
	select {
	case <-entered:
		t.Fatal("file merge entered while the crashed daemon's plane lease remained live")
	default:
	}
	if err := withClaimLock(claimsLockPath, "test.release-crashed-plane-lease", func() error {
		ledger, err := localscheduler.OpenClaimLedger(filepath.Join(plane.layout.SchedulerDir(), claimLedgerFileName))
		if err != nil {
			return err
		}
		return ledger.ReleaseScoped(claimKey(request), request.RunID, claimrelease.Completed.Reason())
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	default:
		t.Fatal("file merge did not enter after the surviving plane lease was released")
	}
}

func requestClaimKey(request httpapi.ClaimRequest) claimsclient.Key {
	return claimsclient.Key{
		Gaggle: request.Gaggle, Provider: request.Provider, ExternalID: request.ItemID,
	}
}
