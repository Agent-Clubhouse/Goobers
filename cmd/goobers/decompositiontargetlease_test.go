package main

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/decomposition"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

const decompositionTargetLeaseTestGaggle = "g"

// failingPublisherProvider satisfies decomposition.PublisherProvider well
// enough to force Publisher.Publish to fail immediately after it acquires
// its target lease (Publish's very next call is GetWorkItem on the parent),
// without needing a full working fake provider.
type failingPublisherProvider struct{ err error }

func (p failingPublisherProvider) GetWorkItem(context.Context, providers.RepositoryRef, string) (providers.WorkItem, error) {
	return providers.WorkItem{}, p.err
}
func (failingPublisherProvider) ListComments(context.Context, providers.RepositoryRef, string) ([]providers.Comment, error) {
	return nil, nil
}
func (failingPublisherProvider) CreateWorkItem(context.Context, providers.CreateWorkItemRequest) (providers.WorkItem, error) {
	return providers.WorkItem{}, errors.New("unused")
}
func (failingPublisherProvider) FindWorkItemsByMarker(context.Context, providers.RepositoryRef, string) ([]providers.WorkItem, error) {
	return nil, nil
}
func (failingPublisherProvider) CreateWorkItemComment(context.Context, providers.RepositoryRef, string, string) (providers.Comment, error) {
	return providers.Comment{}, errors.New("unused")
}
func (failingPublisherProvider) UpdateWorkItem(context.Context, providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	return providers.WorkItem{}, errors.New("unused")
}
func (failingPublisherProvider) ReleaseWorkItemClaim(context.Context, providers.ClaimWorkItemRequest) (providers.WorkItem, error) {
	return providers.WorkItem{}, errors.New("unused")
}

func decompositionTargetLeaseTestPlan(repo providers.RepositoryRef, parentID string) decomposition.Plan {
	repository := repo.Owner + "/" + repo.Name
	return decomposition.Plan{
		Parent: decomposition.ParentRef{Provider: string(repo.Provider), Repository: repository, ID: parentID},
	}
}

// TestClaimsPlaneTargetLeaserDistinctKeyPreservesParentClaimOnPublishFailure
// is #4340's own acceptance evidence for the defect its parked review found
// and rejected the prior attempt over: a target lease that shared its key
// with the parent's own claim would release that claim too the moment
// Publisher.Publish's target-lease defer fires — including on FAILURE, not
// only success. select-source's parent claim (keyed by the item id
// directly) must survive a publish attempt that fails right after
// acquiring the target lease, proving the two claims are on genuinely
// distinct keys rather than merely exercising the lease in isolation.
func TestClaimsPlaneTargetLeaserDistinctKeyPreservesParentClaimOnPublishFailure(t *testing.T) {
	// Bounded: a shared-key regression makes the target lease contend
	// forever against the already-held parent claim, which would otherwise
	// hang this test (and the whole package's test run) instead of failing
	// it — confirmed by temporarily reintroducing that exact bug.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	layout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "widgets"}
	const parentID = "42"

	ledger, err := fileClaimLedger(layout)
	if err != nil {
		t.Fatalf("fileClaimLedger: %v", err)
	}
	parentKey := claimsclient.Key{Gaggle: decompositionTargetLeaseTestGaggle, Provider: string(repo.Provider), ExternalID: parentID}
	ok, _, err := ledger.ClaimScoped(ctx, parentKey, "select-source-run", "implementation", time.Hour)
	if err != nil || !ok {
		t.Fatalf("claim parent: ok=%v err=%v", ok, err)
	}

	forcedErr := errors.New("forced provider failure")
	publisher := decomposition.Publisher{
		Provider: failingPublisherProvider{err: forcedErr},
		Leaser:   newDecompositionTargetLeaser(layout, decompositionTargetLeaseTestGaggle, "publish-run"),
		Repo:     repo,
		RunID:    "publish-run",
	}
	_, publishErr := publisher.Publish(ctx, decompositionTargetLeaseTestPlan(repo, parentID))
	if !errors.Is(publishErr, forcedErr) {
		t.Fatalf("Publish error = %v, want it to wrap %v", publishErr, forcedErr)
	}

	held, err := ledger.ForRunAll(ctx, "select-source-run")
	if err != nil {
		t.Fatalf("ForRunAll: %v", err)
	}
	if len(held) != 1 || held[0].Gaggle != parentKey.Gaggle || held[0].Provider != parentKey.Provider || held[0].ExternalID != parentKey.ExternalID {
		t.Fatalf("select-source-run's held claims = %+v, want the parent claim %+v still held after the failed publish", held, parentKey)
	}

	// The target lease itself must have been released despite the failure —
	// Publisher.Publish's defer always runs — proving it is a real, distinct,
	// non-leaked claim rather than one that merely never got acquired.
	targetKey := claimsclient.Key{Gaggle: decompositionTargetLeaseTestGaggle, Provider: string(repo.Provider), ExternalID: decompositionTargetLeaseExternalID(parentID)}
	reacquired, holder, err := ledger.ClaimScoped(ctx, targetKey, "another-run", "decomposition-target-lease", time.Minute)
	if err != nil {
		t.Fatalf("re-acquire target lease: %v", err)
	}
	if !reacquired {
		t.Fatalf("target lease held by %q after Publish returned; the failure-path release did not run", holder)
	}
}

// TestClaimsPlaneTargetLeaserFileAndPlaneAgree proves the leaser behaves
// identically whether it selects the file backend (a self runner) or the
// claims plane (a stage pod) — the shared-seam parity #4340 asks for,
// reusing the SAME production selection openStageClaimLedger already
// performs for every other pod-capable claim rather than asserting it as a
// belief about the code.
func TestClaimsPlaneTargetLeaserFileAndPlaneAgree(t *testing.T) {
	ctx := context.Background()
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "widgets"}
	const itemID = "7"

	run := func(t *testing.T, layout instance.Layout, runID string) (release func() error) {
		t.Helper()
		leaser := newDecompositionTargetLeaser(layout, decompositionTargetLeaseTestGaggle, runID)
		release, err := leaser.Acquire(ctx, repo, itemID)
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		return release
	}

	// SELF: no claims-plane environment, so openStageClaimLedger selects the
	// instance's own file-backed ledger.
	fileLayout := instance.NewLayout(t.TempDir())
	release := run(t, fileLayout, "self-run")
	if err := release(); err != nil {
		t.Fatalf("file-backed release: %v", err)
	}
	// A second acquire after release must succeed immediately — proves the
	// first release actually cleared the lease rather than merely returning.
	release2 := run(t, fileLayout, "self-run-2")
	if err := release2(); err != nil {
		t.Fatalf("file-backed second release: %v", err)
	}

	// POD: the claims plane is stamped into the environment, so the SAME
	// leaser construction and the SAME production seam select the HTTP
	// backend instead — a pod's layout carries no scheduler dir of its own.
	plane := newClaimsPlane(t)
	token := plane.admitRun(t, decompositionTargetLeaseTestGaggle, "pod-run")
	stampClaimsPlaneEnv(t, plane, "pod-run", token)
	podLayout := instance.NewLayout(t.TempDir())
	podRelease := run(t, podLayout, "pod-run")
	if err := podRelease(); err != nil {
		t.Fatalf("plane-backed release: %v", err)
	}
}

// TestClaimsPlaneTargetLeaserWaitsForContendedLease is FileTargetLeaser's
// pre-existing contract, preserved: a second acquirer of the SAME target
// waits for the first to release rather than being refused outright — a
// target lease serializes two concurrent publishers of the same batch
// (self and pod), it does not park one of them for later like an ordinary
// work-item claim would.
func TestClaimsPlaneTargetLeaserWaitsForContendedLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	layout := instance.NewLayout(t.TempDir())
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "widgets"}

	first := newDecompositionTargetLeaser(layout, decompositionTargetLeaseTestGaggle, "run-1")
	first.pollInterval = 5 * time.Millisecond
	releaseFirst, err := first.Acquire(ctx, repo, "99")
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	second := newDecompositionTargetLeaser(layout, decompositionTargetLeaseTestGaggle, "run-2")
	second.pollInterval = 5 * time.Millisecond
	done := make(chan error, 1)
	go func() {
		_, err := second.Acquire(ctx, repo, "99")
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("second Acquire returned (err=%v) before the first released", err)
	case <-time.After(50 * time.Millisecond):
	}

	if err := releaseFirst(); err != nil {
		t.Fatalf("release first: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second Acquire after release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Acquire never returned after the first released")
	}
}

// TestClaimsPlaneTargetLeaserRecoversExpiredLease is #4340's crash-recovery
// acceptance criterion: a lease from a run that crashed without releasing
// still expires and becomes acquirable, with no manual claims.json cleanup.
func TestClaimsPlaneTargetLeaserRecoversExpiredLease(t *testing.T) {
	ctx := context.Background()
	layout := instance.NewLayout(t.TempDir())
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "widgets"}

	crashed := newDecompositionTargetLeaser(layout, decompositionTargetLeaseTestGaggle, "crashed-run")
	crashed.leaseDuration = 20 * time.Millisecond
	if _, err := crashed.Acquire(ctx, repo, "5"); err != nil {
		t.Fatalf("crashed run Acquire: %v", err)
	}
	// No release call: this simulates the crash.

	time.Sleep(50 * time.Millisecond)

	recovering := newDecompositionTargetLeaser(layout, decompositionTargetLeaseTestGaggle, "recovering-run")
	recovering.pollInterval = 5 * time.Millisecond
	acquireCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	release, err := recovering.Acquire(acquireCtx, repo, "5")
	if err != nil {
		t.Fatalf("recovering run Acquire after expiry: %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
}
