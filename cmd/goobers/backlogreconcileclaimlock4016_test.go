package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/stateclient"
	"github.com/goobers/goobers/providers"
)

// backlogreconcileclaimlock4016_test.go is Goobers#4016's evidence.
//
// `backlog-curation`'s reconcile-backlog-metadata stage begins by reaping
// stale claim-ledger leases, so that a provider claim marker left behind by a
// dead claimant can be cleared in the same pass. That sweep used to be an
// in-process recoverClaims call, which opens SchedulerDir()/claims.lock by
// path.
//
// For as long as the lane was refused pod placement, that was invisible. The
// moment #3992 let a pod-pinned, engine-selected workflow actually dispatch,
// the stage ran in a pod with no GOOBERS_INSTANCE_ROOT, providerStageRoot
// fell through to ".", and every scheduled run died on
//
//	recover stale claims before metadata reconciliation: acquire claims lock:
//	lock: open "scheduler/claims.lock": no such file or directory
//
// against the container's working directory. Each test below asserts either
// that the pod and the daemon advance ONE ledger, or that a configuration
// which cannot sweep says so out loud instead of resolving a relative path.

const reconcileClaimGaggle = "goobers"

// reconcilePlanes is the daemon a pod-dispatched reconcile stage talks to:
// one instance layout behind BOTH the claims plane (the sweep, the claim
// reservations) and the scheduler-state plane (the learned-block ledger the
// stage reads next), which is exactly the pair the dispatcher stamps into a
// goobers-CLI stage pod.
type reconcilePlanes struct {
	server   *httptest.Server
	layout   instance.Layout
	registry *podauth.Registry
}

func newReconcilePlanes(t *testing.T) *reconcilePlanes {
	t.Helper()
	layout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	instanceLog, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instanceLog.Close() })
	stateService, err := newDaemonStateService(layout)
	if err != nil {
		t.Fatal(err)
	}
	registry := podauth.NewRegistry()
	authenticator, err := podauth.NewAuthenticator(registry, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0),
		httpapi.WithAuthenticator(authenticator),
		httpapi.WithStateService(stateService),
		// The daemon's OWN sweep, as up.go wires it.
		httpapi.WithClaimService(newDaemonClaimService(layout, instanceLog, func(now time.Time) ([]localscheduler.ClaimEntry, error) {
			return recoverClaims(layout, instanceLog, now, nil, nil)
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &reconcilePlanes{server: server, layout: layout, registry: registry}
}

func (p *reconcilePlanes) admitRun(t *testing.T, runID string) string {
	t.Helper()
	run, err := journal.Create(
		p.layout.ForGaggle(reconcileClaimGaggle).RunsDir(),
		journal.RunIdentity{RunID: runID, Workflow: "backlog-curation", Gaggle: reconcileClaimGaggle},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	token, err := p.registry.Mint(runID, 0)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// stampStagePodEnv is the environment the dispatcher gives a goobers-CLI
// stage pod for the two planes this stage needs.
func (p *reconcilePlanes) stampStagePodEnv(t *testing.T, runID, token string) {
	t.Helper()
	t.Setenv(claimsclient.EnvEndpoint, p.server.URL)
	t.Setenv(claimsclient.EnvToken, token)
	t.Setenv(claimsclient.EnvRunID, runID)
	t.Setenv(stateclient.EnvEndpoint, p.server.URL)
	t.Setenv(stateclient.EnvToken, token)
	t.Setenv(stateclient.EnvGaggle, reconcileClaimGaggle)
	t.Setenv("GOOBERS_GAGGLE", reconcileClaimGaggle)
}

// seedExpiredLease puts a lease on the DAEMON's ledger that is already past
// its expiry, so only a real sweep can remove it.
func (p *reconcilePlanes) seedExpiredLease(t *testing.T, itemID, runID string) localscheduler.ClaimKey {
	t.Helper()
	past := time.Now().Add(-2 * time.Hour)
	ledger, err := localscheduler.OpenClaimLedger(
		filepath.Join(p.layout.SchedulerDir(), claimLedgerFileName),
		localscheduler.WithLedgerClock(func() time.Time { return past }),
	)
	if err != nil {
		t.Fatal(err)
	}
	key := localscheduler.ClaimKey{
		Gaggle:     reconcileClaimGaggle,
		Provider:   string(providers.ProviderGitHub),
		ExternalID: itemID,
	}
	if ok, _, err := ledger.ClaimScoped(key, runID, "implementation", time.Minute); err != nil || !ok {
		t.Fatalf("seed expired lease: ok=%v err=%v", ok, err)
	}
	return key
}

func (p *reconcilePlanes) leaseHeld(t *testing.T, key localscheduler.ClaimKey) bool {
	t.Helper()
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(p.layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	_, held := ledger.LookupScoped(key)
	return held
}

// podStageLayout is the layout a pod-dispatched provider-chain subcommand
// actually resolves: providerStageRoot found neither a path argument nor
// GOOBERS_INSTANCE_ROOT, so the root addresses a directory that is not an
// instance at all. Any test that finds a scheduler directory under it has
// caught the sweep falling back to a local lock the pod does not have.
func podStageLayout(t *testing.T) instance.Layout {
	t.Helper()
	return instance.NewLayout(t.TempDir())
}

func assertNoStageSchedulerDir(t *testing.T, pod instance.Layout) {
	t.Helper()
	if _, err := os.Stat(pod.SchedulerDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the stage created %s (err = %v); a plane-routed sweep must never touch a local scheduler directory",
			pod.SchedulerDir(), err)
	}
}

// TestStageClaimRecoveryInAPodSweepsOnTheDaemon is the direct regression: the
// seam the reconcile stage calls, in a pod's environment, over a root that is
// not an instance. It must not fail, must not create a scheduler directory,
// and must leave the DAEMON's ledger actually swept — the sweep happened,
// somewhere real.
func TestStageClaimRecoveryInAPodSweepsOnTheDaemon(t *testing.T) {
	planes := newReconcilePlanes(t)
	token := planes.admitRun(t, "pod-run")
	key := planes.seedExpiredLease(t, "21", "expired-run")
	planes.stampStagePodEnv(t, "pod-run", token)

	pod := podStageLayout(t)
	if err := recoverStageClaims(pod, time.Now()); err != nil {
		t.Fatalf("recoverStageClaims in a pod: %v", err)
	}
	assertNoStageSchedulerDir(t, pod)
	if planes.leaseHeld(t, key) {
		t.Fatal("the expired lease survived; the pod's request did not reach the daemon's sweep")
	}
}

// TestReconcileBacklogMetadataSucceedsWhenPodDispatched is the live failure
// itself, end to end: reconcileBacklogMetadata over a pod-shaped root, with a
// dead claimant's marker on a real (fake-server) backlog. Before the fix this
// returned "recover stale claims before metadata reconciliation: acquire
// claims lock: ... no such file or directory" and reconciled nothing.
func TestReconcileBacklogMetadataSucceedsWhenPodDispatched(t *testing.T) {
	planes := newReconcilePlanes(t)
	token := planes.admitRun(t, "pod-run")
	key := planes.seedExpiredLease(t, "21", "expired-run")

	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(21, "Expired claim", "goobers:approved", providers.LabelReady, providers.LabelClaimed)
	planes.stampStagePodEnv(t, "pod-run", token)

	pod := podStageLayout(t)
	now := time.Date(2026, 8, 31, 5, 17, 24, 0, time.UTC)
	reconciled, err := reconcileBacklogMetadata(
		context.Background(),
		pod,
		server.newGitHubProvider("token"),
		providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"},
		"goobers:approved",
		defaultBacklogStalenessPolicy(),
		func() time.Time { return now },
	)
	if err != nil {
		t.Fatalf("reconcileBacklogMetadata pod-dispatched: %v", err)
	}
	assertNoStageSchedulerDir(t, pod)
	if planes.leaseHeld(t, key) {
		t.Fatal("the expired lease survived the reconcile stage's sweep")
	}
	if reconciled != 1 {
		t.Fatalf("reconciliations = %d, want the one orphaned claim marker cleared", reconciled)
	}
	assertFakeIssueLabels(t, server, 21, []string{providers.LabelReady}, []string{providers.LabelClaimed})}

// TestStageClaimRecoveryOffThePlaneStillSweepsInProcess is the other half of
// the seam: a mode-1/mode-2 instance, or a self-placed stage running as a
// daemon subprocess, has no claims endpoint and must keep doing exactly what
// it always did — open the instance's own lock and sweep in process.
func TestStageClaimRecoveryOffThePlaneStillSweepsInProcess(t *testing.T) {
	t.Setenv(claimsclient.EnvEndpoint, "")
	t.Setenv(claimsclient.EnvToken, "")
	root := initDemo(t)
	layout := layoutFor(root)

	past := time.Now().Add(-2 * time.Hour)
	seed, err := localscheduler.OpenClaimLedger(
		filepath.Join(layout.SchedulerDir(), claimLedgerFileName),
		localscheduler.WithLedgerClock(func() time.Time { return past }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := seed.Claim("issue-1", "expired-run", "implementation", time.Minute); err != nil || !ok {
		t.Fatalf("seed expired lease: ok=%v err=%v", ok, err)
	}

	if err := recoverStageClaims(layout, time.Now()); err != nil {
		t.Fatalf("recoverStageClaims off the plane: %v", err)
	}
	reopened, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, held := reopened.Lookup("issue-1"); held {
		t.Fatal("the expired lease survived the in-process sweep")
	}
}

// TestStageClaimRecoveryRefusesARootlessInvocation is the fail-loud half of
// the fix, and it is the one that keeps this defect from recurring in some
// other shape. A stage that has NEITHER a claims endpoint NOR an instance
// root cannot sweep anything; before, it silently resolved
// "scheduler/claims.lock" against the container's working directory and
// reported an ENOENT that named a path nobody had configured. Now it names
// the two variables that would have made it work, and it creates nothing.
func TestStageClaimRecoveryRefusesARootlessInvocation(t *testing.T) {
	t.Setenv(claimsclient.EnvEndpoint, "")
	t.Setenv(claimsclient.EnvToken, "")

	pod := podStageLayout(t)
	err := recoverStageClaims(pod, time.Now())
	if err == nil {
		t.Fatal("a rootless, planeless sweep reported success")
	}
	if !errors.Is(err, errStageClaimRecoveryRootless) {
		t.Fatalf("err = %v, want the explicit rootless refusal", err)
	}
	for _, fragment := range []string{claimsclient.EnvEndpoint, "GOOBERS_INSTANCE_ROOT"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("refusal %q does not name %s", err.Error(), fragment)
		}
	}
	assertNoStageSchedulerDir(t, pod)
}

// TestStageClaimRecoveryFailsClosedOnAHalfConfiguredClaimsPlane pins the
// third configuration: an endpoint with no bearer. claimsclient.Select
// already refuses that rather than falling back to a file ledger the pod does
// not have, and the sweep must surface the refusal rather than quietly
// skipping recovery.
func TestStageClaimRecoveryFailsClosedOnAHalfConfiguredClaimsPlane(t *testing.T) {
	pod := podStageLayout(t)
	for name, env := range map[string]struct{ token, runID string }{
		"no bearer":       {token: "", runID: "pod-run"},
		"no run identity": {token: "claims-token", runID: ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(claimsclient.EnvEndpoint, "http://daemon.invalid:7777")
			t.Setenv(claimsclient.EnvToken, env.token)
			t.Setenv(claimsclient.EnvRunID, env.runID)
			if err := recoverStageClaims(pod, time.Now()); err == nil {
				t.Fatal("a half-configured claims plane reported a successful sweep")
			}
			assertNoStageSchedulerDir(t, pod)
		})
	}
}
