package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/providers"
)

// claimsPlane is the daemon's REAL claims plane: podauth in front of a
// deny-all human fallback, httpapi.RequireRoles as the authorizer, and
// daemonClaimService over an instance layout behind httpapi.WithClaimService
// — the construction cmd/goobers/up.go performs. The ledger file under the
// layout is the daemon side of the seam.
type claimsPlane struct {
	server   *httptest.Server
	layout   instance.Layout
	registry *podauth.Registry
}

func newClaimsPlane(t *testing.T) *claimsPlane {
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
	registry := podauth.NewRegistry()
	authenticator, err := podauth.NewAuthenticator(registry, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0),
		httpapi.WithAuthenticator(authenticator),
		httpapi.WithClaimService(newDaemonClaimService(layout, instanceLog)),
	)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &claimsPlane{server: server, layout: layout, registry: registry}
}

// admitRun creates runID's journal under gaggle on the daemon (the run.yaml
// the live writer creates at a run's first emit) and mints its pod token.
func (p *claimsPlane) admitRun(t *testing.T, gaggle, runID string) string {
	t.Helper()
	run, err := journal.Create(p.layout.ForGaggle(gaggle).RunsDir(), journal.RunIdentity{RunID: runID, Workflow: "implementation", Gaggle: gaggle}, nil)
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

// client is the plane backend a stage pod for runID would construct.
func (p *claimsPlane) client(t *testing.T, runID, token string) claimsclient.Ledger {
	t.Helper()
	client, err := claimsclient.NewHTTP(claimsclient.HTTPConfig{
		BaseURL: p.server.URL, Token: token, RunID: runID,
		MergeLockPoll: 5 * time.Millisecond, MergeLockLease: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// stampClaimsPlaneEnv is the environment a goobers-CLI stage pod carries for
// the claims plane (the dispatcher's R2 stamping), so the production
// selection seam picks the plane backend.
func stampClaimsPlaneEnv(t *testing.T, plane *claimsPlane, runID, token string) {
	t.Helper()
	t.Setenv(claimsclient.EnvEndpoint, plane.server.URL)
	t.Setenv(claimsclient.EnvToken, token)
	t.Setenv(claimsclient.EnvRunID, runID)
}

func normalizeClaimEntries(entries []claimsclient.Entry) []claimsclient.Entry {
	out := make([]claimsclient.Entry, 0, len(entries))
	for _, entry := range entries {
		entry.ClaimedAt, entry.ExpiresAt = time.Time{}, time.Time{}
		if entry.ReleasedAt != nil {
			released := time.Time{}
			entry.ReleasedAt = &released
		}
		out = append(out, entry)
	}
	return out
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestClaimLedgerFileAndPlaneParity is the file-vs-HTTP parity suite for
// every primitive (finding 002 C1): the same script of claims, refusals,
// reads and releases runs once against the instance's file ledger (the
// self-runner path, under withClaimLock) and once through the daemon's
// claims plane from a pod-shaped client, and every answer and the resulting
// ledger state must agree. What the file backend proves byte-identical to
// the ledger (its own tests), this proves the plane is identical to the
// file — so a stage moved into a pod claims exactly as it did on the daemon.
func TestClaimLedgerFileAndPlaneParity(t *testing.T) {
	ctx := context.Background()
	const gaggle, provider = "g", "github"
	type outcome struct {
		Acquired           bool
		Holder             string
		Contended          bool
		ContendedHolder    string
		Renewed            bool
		HeldByRun          []claimsclient.Entry
		Namespace          []claimsclient.Entry
		ReleasedAll        []claimsclient.Entry
		HistoryAfter       []claimsclient.Entry
		NonHolderReleaseOK bool
		Ledger             []claimsclient.Entry
	}
	script := func(t *testing.T, ledger claimsclient.Ledger, otherRun claimsclient.Ledger) outcome {
		t.Helper()
		var out outcome
		item := claimsclient.Key{Gaggle: gaggle, Provider: provider, ExternalID: "42"}
		second := claimsclient.Key{Gaggle: gaggle, Provider: provider, ExternalID: "43"}
		err := ledger.Locked(ctx, claimLockOperationBacklogClaim, func(tx claimsclient.Ledger) error {
			var err error
			out.Acquired, out.Holder, err = tx.ClaimScoped(ctx, item, "run-1", "implementation", 30*time.Minute)
			if err != nil {
				return err
			}
			_, _, err = tx.ClaimScoped(ctx, second, "run-1", "implementation", 30*time.Minute)
			return err
		})
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		out.Contended, out.ContendedHolder, err = otherRun.ClaimScoped(ctx, item, "run-2", "implementation", 30*time.Minute)
		if err != nil {
			t.Fatalf("contended acquire: %v", err)
		}
		out.Renewed, _, err = ledger.ClaimScoped(ctx, item, "run-1", "implementation", time.Hour)
		if err != nil {
			t.Fatalf("renew: %v", err)
		}
		held, err := ledger.ForRunAll(ctx, "run-1")
		if err != nil {
			t.Fatalf("ForRunAll: %v", err)
		}
		out.HeldByRun = normalizeClaimEntries(held)
		listing, err := ledger.ListNamespace(ctx, gaggle, provider)
		if err != nil {
			t.Fatalf("ListNamespace: %v", err)
		}
		out.Namespace = normalizeClaimEntries(listing.Entries)
		// A non-holder's release is the ledger's idempotent no-op.
		if err := otherRun.ReleaseScoped(ctx, item, "run-2"); err != nil {
			t.Fatalf("non-holder release: %v", err)
		}
		stillHeld, err := ledger.ForRunAll(ctx, "run-1")
		if err != nil {
			t.Fatal(err)
		}
		out.NonHolderReleaseOK = len(stillHeld) == 2
		if err := ledger.ReleaseScoped(ctx, second, "run-1"); err != nil {
			t.Fatalf("release: %v", err)
		}
		released, err := ledger.ReleaseAllForRun(ctx, "run-1")
		if err != nil {
			t.Fatalf("ReleaseAllForRun: %v", err)
		}
		out.ReleasedAll = normalizeClaimEntries(released)
		after, err := ledger.ListNamespace(ctx, gaggle, provider)
		if err != nil {
			t.Fatal(err)
		}
		out.HistoryAfter = normalizeClaimEntries(after.HistoryForItem("42"))
		out.Ledger = normalizeClaimEntries(after.Entries)
		return out
	}

	// File: the self-runner path — the instance's ledger under withClaimLock.
	fileLayout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(fileLayout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	fileLedger, err := fileClaimLedger(fileLayout)
	if err != nil {
		t.Fatal(err)
	}
	viaFile := script(t, fileLedger, fileLedger)

	// Plane: two pod-shaped clients (one per run) against one daemon.
	plane := newClaimsPlane(t)
	run1 := plane.client(t, "run-1", plane.admitRun(t, gaggle, "run-1"))
	run2 := plane.client(t, "run-2", plane.admitRun(t, gaggle, "run-2"))
	viaPlane := script(t, run1, run2)

	if got, want := mustJSON(t, viaPlane), mustJSON(t, viaFile); got != want {
		t.Fatalf("plane and file disagree:\n plane: %s\n file:  %s", got, want)
	}
	if !viaFile.Acquired || viaFile.Contended || viaFile.ContendedHolder != "run-1" || !viaFile.Renewed ||
		len(viaFile.HeldByRun) != 2 || len(viaFile.Namespace) != 2 ||
		!viaFile.NonHolderReleaseOK || len(viaFile.ReleasedAll) != 1 || len(viaFile.HistoryAfter) != 1 ||
		viaFile.HistoryAfter[0].ReleasedAt == nil || len(viaFile.Ledger) != 0 {
		t.Fatalf("script outcome is not the expected ledger behaviour: %s", mustJSON(t, viaFile))
	}

	// The daemon's ledger — not the pod's filesystem — holds the record, and
	// its instance journal names the pod principal's run.
	daemonLedger, err := localscheduler.OpenClaimLedger(filepath.Join(plane.layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if history := daemonLedger.HistoryForRun("run-1"); len(history) != 2 {
		t.Fatalf("daemon ledger history for run-1 = %+v, want both released claims", history)
	}
	events, err := journal.ReadInstanceLog(plane.layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var acquired, refused, released int
	for _, event := range events {
		switch event.Type {
		case journal.EventClaimAcquired:
			if event.RunID == "run-1" {
				acquired++
			}
		case journal.EventClaimRefused:
			if event.RunID == "run-2" && event.Runner["holder"] == "run-1" {
				refused++
			}
		case journal.EventClaimReleased:
			if event.RunID == "run-1" {
				released++
			}
		}
	}
	if acquired < 2 || refused != 1 || released != 2 {
		t.Fatalf("daemon instance journal: acquired = %d, refused = %d, released = %d", acquired, refused, released)
	}
}

// TestClaimLedgerPlaneContainsPodToItsRun pins the plane's containment as
// seen from the client: a pod may act only as its own run, and may list only
// the namespace its run belongs to.
func TestClaimLedgerPlaneContainsPodToItsRun(t *testing.T) {
	ctx := context.Background()
	plane := newClaimsPlane(t)
	token := plane.admitRun(t, "g", "run-1")
	asOther := plane.client(t, "run-1", token)
	key := claimsclient.Key{Gaggle: "g", Provider: "github", ExternalID: "1"}

	var planeErr *claimsclient.Error
	if _, _, err := asOther.ClaimScoped(ctx, key, "run-2", "w", time.Hour); !errors.As(err, &planeErr) || planeErr.Code != "run_mismatch" {
		t.Fatalf("claim as another run: err = %v, want run_mismatch", err)
	}
	if _, err := asOther.ForRunAll(ctx, "run-2"); !errors.As(err, &planeErr) || planeErr.Code != "run_mismatch" {
		t.Fatalf("list another run: err = %v, want run_mismatch", err)
	}
	if _, err := asOther.ReleaseAllForRun(ctx, "run-2"); !errors.As(err, &planeErr) || planeErr.Code != "run_mismatch" {
		t.Fatalf("release-all for another run: err = %v, want run_mismatch", err)
	}
	if _, err := asOther.ListNamespace(ctx, "other-gaggle", "github"); !errors.As(err, &planeErr) || planeErr.Code != "gaggle_mismatch" {
		t.Fatalf("list another gaggle's namespace: err = %v, want gaggle_mismatch", err)
	}
	if _, err := asOther.ListNamespace(ctx, "g", "github"); err != nil {
		t.Fatalf("list own namespace: %v", err)
	}
	unknown := plane.client(t, "run-1", "goobers-pod.deadbeef")
	if _, err := unknown.ForRunAll(ctx, "run-1"); !errors.As(err, &planeErr) || planeErr.Status != 401 {
		t.Fatalf("unknown bearer: err = %v, want 401", err)
	}
}

// TestClaimLedgerMergeLockParity pins the merge window on both backends: the
// file backend serializes through the instance-wide merge flock, the plane
// through a per-repository lease — and on both, a second claimant enters
// the window only after the first leaves it.
func TestClaimLedgerMergeLockParity(t *testing.T) {
	ctx := context.Background()
	backends := map[string]func(t *testing.T) (claimsclient.Ledger, claimsclient.Ledger){
		"file": func(t *testing.T) (claimsclient.Ledger, claimsclient.Ledger) {
			layout := instance.NewLayout(t.TempDir())
			if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := instance.WriteConfig(layout.ConfigFile(), &instance.Config{}); err != nil {
				t.Fatal(err)
			}
			first, err := fileClaimLedger(layout)
			if err != nil {
				t.Fatal(err)
			}
			second, err := fileClaimLedger(layout)
			if err != nil {
				t.Fatal(err)
			}
			return first, second
		},
		"plane": func(t *testing.T) (claimsclient.Ledger, claimsclient.Ledger) {
			plane := newClaimsPlane(t)
			return plane.client(t, "run-1", plane.admitRun(t, "g", "run-1")),
				plane.client(t, "run-2", plane.admitRun(t, "g", "run-2"))
		},
	}
	for name, build := range backends {
		t.Run(name, func(t *testing.T) {
			first, second := build(t)
			lock := func(runID string) claimsclient.MergeLock {
				return claimsclient.MergeLock{Key: claimsclient.MergeLockKey("g", "github", "acme", "web"), RunID: runID, Workflow: "merge-review"}
			}
			var mu sync.Mutex
			var order []string
			entered := make(chan struct{})
			release := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = first.MergeLock(ctx, lock("run-1"), func() error {
					mu.Lock()
					order = append(order, "first-in")
					mu.Unlock()
					close(entered)
					<-release
					mu.Lock()
					order = append(order, "first-out")
					mu.Unlock()
					return nil
				})
			}()
			<-entered
			secondDone := make(chan error, 1)
			go func() {
				secondDone <- second.MergeLock(ctx, lock("run-2"), func() error {
					mu.Lock()
					order = append(order, "second-in")
					mu.Unlock()
					return nil
				})
			}()
			select {
			case err := <-secondDone:
				t.Fatalf("second claimant entered the window while the first held it (err = %v)", err)
			case <-time.After(100 * time.Millisecond):
			}
			close(release)
			wg.Wait()
			if err := <-secondDone; err != nil {
				t.Fatalf("second claimant: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if want := []string{"first-in", "first-out", "second-in"}; mustJSON(t, order) != mustJSON(t, want) {
				t.Fatalf("window order = %v, want %v", order, want)
			}
		})
	}
}

// TestStageClaimLedgerFailsClosedWithoutToken pins the stage seam's
// fail-closed selection: an endpoint in the environment with no bearer is
// an error from every seam, never a silent fall-through to the (absent)
// pod-local ledger file.
func TestStageClaimLedgerFailsClosedWithoutToken(t *testing.T) {
	t.Setenv(claimsclient.EnvEndpoint, "http://daemon.invalid")
	t.Setenv(claimsclient.EnvToken, "")
	t.Setenv(claimsclient.EnvRunID, "run-1")
	layout := instance.NewLayout(t.TempDir())
	if _, err := stageClaimLedger(layout); !errors.Is(err, claimsclient.ErrEndpointWithoutToken) {
		t.Fatalf("stageClaimLedger: err = %v, want ErrEndpointWithoutToken", err)
	}
	if _, err := stageClaimLedgerForRun(layout, "g", "run-1"); !errors.Is(err, claimsclient.ErrEndpointWithoutToken) {
		t.Fatalf("stageClaimLedgerForRun: err = %v, want ErrEndpointWithoutToken", err)
	}
	if log, closeLog, err := claimLedgerJournal(layout); err != nil || log != nil {
		t.Fatalf("claimLedgerJournal on the plane = %v, %v; want no instance log and no error", log, err)
	} else {
		closeLog()
	}
	// The daemon's own file constructions never consult the environment.
	if _, err := fileClaimLedger(layout); err != nil {
		t.Fatalf("fileClaimLedger consulted the stage environment: %v", err)
	}
	if _, err := heldClaimLedger(layout); err != nil {
		t.Fatalf("heldClaimLedger consulted the stage environment: %v", err)
	}
}

// TestBacklogDedupeReadsClaimsOverThePlane is the seam proof: with the
// claims plane in its environment, `goobers backlog-dedupe` learns this
// run's claimed items from the DAEMON's ledger — the local instance root's
// ledger is empty, and without the plane the artifact would (silently)
// report nothing claimed, the exact wrong-result class the refusal list
// exists to close.
func TestBacklogDedupeReadsClaimsOverThePlane(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(101, "Claimed account sync failure", "goobers:approved")
	server.addIssue(900, "Customer import conflict")
	setFakeIssueBody(t, server, 101, "External ref OPS-4421 contains the failing request.")
	setFakeIssueBody(t, server, 900, "Investigate OPS-4421 from the customer report.")

	plane := newClaimsPlane(t)
	token := plane.admitRun(t, "goobers", "curation-run")
	daemonLedger, err := localscheduler.OpenClaimLedger(filepath.Join(plane.layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := daemonLedger.ClaimScoped(localscheduler.ClaimKey{Gaggle: "goobers", Provider: "github", ExternalID: "101"}, "curation-run", "backlog-curation", time.Hour); err != nil || !ok {
		t.Fatalf("seed daemon claim: ok=%v err=%v", ok, err)
	}

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_READ", "curation-run")
	t.Setenv("GOOBERS_WORKFLOW", "backlog-curation")
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	stampClaimsPlaneEnv(t, plane, "curation-run", token)
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "backlog-dedupe", root)
	if code != 0 {
		t.Fatalf("backlog-dedupe: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "dedupe-candidates.json"))
	if err != nil {
		t.Fatal(err)
	}
	var artifact dedupeCandidateArtifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatal(err)
	}
	if mustJSON(t, artifact.ClaimedIDs) != `["101"]` {
		t.Fatalf("claimedIds = %v, want the daemon-held item 101", artifact.ClaimedIDs)
	}
	if _, err := os.Stat(filepath.Join(root, "scheduler", claimLedgerFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the stage's local root grew a claims.json (err = %v); the plane must be the only ledger a pod touches", err)
	}
}

// TestBacklogReconcileReservationOverThePlane pins the --reconcile /
// --feedback reservation's plane shape (finding 002 C1 + the critic's
// containment row): over the plane the reservation is taken under the run's
// OWN id — the only id the bearer may act as — and an item the run already
// holds is reported not-reservable, exactly as the synthesized reservation
// id was refused by the run's own claim on the file path.
func TestBacklogReconcileReservationOverThePlane(t *testing.T) {
	plane := newClaimsPlane(t)
	token := plane.admitRun(t, "goobers", "curation-run")
	daemonLedger, err := localscheduler.OpenClaimLedger(filepath.Join(plane.layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	held := localscheduler.ClaimKey{Gaggle: "goobers", Provider: "github", ExternalID: "5"}
	if ok, _, err := daemonLedger.ClaimScoped(held, "curation-run", "backlog-curation", time.Hour); err != nil || !ok {
		t.Fatalf("seed held item: ok=%v err=%v", ok, err)
	}
	stampClaimsPlaneEnv(t, plane, "curation-run", token)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	podRoot := instance.NewLayout(t.TempDir())
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}

	if _, acquired, err := reserveBacklogClaimReconciliation(podRoot, repo, "5", time.Now); err != nil || acquired {
		t.Fatalf("reserving an item the run itself holds: acquired=%v err=%v; want not reservable", acquired, err)
	}
	reservation, acquired, err := reserveBacklogClaimReconciliation(podRoot, repo, "6", time.Now)
	if err != nil || !acquired {
		t.Fatalf("reserve 6: acquired=%v err=%v", acquired, err)
	}
	if reservation.runID != "curation-run" {
		t.Fatalf("reservation run id = %q, want the run's own id over the plane", reservation.runID)
	}
	reopened, err := localscheduler.OpenClaimLedger(filepath.Join(plane.layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if entry, ok := reopened.LookupScoped(localscheduler.ClaimKey{Gaggle: "goobers", Provider: "github", ExternalID: "6"}); !ok || entry.RunID != "curation-run" {
		t.Fatalf("daemon ledger entry for 6 = %+v, %v; want held by curation-run", entry, ok)
	}
	if err := releaseBacklogClaimReconciliation(podRoot, *reservation); err != nil {
		t.Fatal(err)
	}
	reopened, err = localscheduler.OpenClaimLedger(filepath.Join(plane.layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.LookupScoped(localscheduler.ClaimKey{Gaggle: "goobers", Provider: "github", ExternalID: "6"}); ok {
		t.Fatal("reservation for 6 was not released on the daemon")
	}
	if _, ok := reopened.LookupScoped(held); !ok {
		t.Fatal("the run's own claim on 5 was released by the reconcile pass")
	}
	if _, err := os.Stat(filepath.Join(podRoot.SchedulerDir(), claimLedgerFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the pod root grew a claims.json (err = %v)", err)
	}
}

// TestPullRequestClaimsOverThePlane pins the pr-claim family's plane shape:
// claimPullRequestInOrder leases on the daemon, claimedPullRequestNumber
// reads the lease back from it, and pr-claim --release surrenders it —
// with no instance log and no ledger file on the pod's own root.
func TestPullRequestClaimsOverThePlane(t *testing.T) {
	plane := newClaimsPlane(t)
	token := plane.admitRun(t, "goobers", "run-1")
	daemonLedger, err := localscheduler.OpenClaimLedger(filepath.Join(plane.layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := daemonLedger.ClaimScoped(localscheduler.ClaimKey{Gaggle: "goobers", Provider: "github", ExternalID: pullRequestClaimKey(77)}, "run-0", "pr-remediation", time.Hour); err != nil || !ok {
		t.Fatalf("seed contended PR: ok=%v err=%v", ok, err)
	}
	stampClaimsPlaneEnv(t, plane, "run-1", token)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	podRoot := t.TempDir()

	selected, err := claimPullRequestInOrder(podRoot, []providers.PullRequestSummary{{Number: 77}, {Number: 78}}, "run-1", "pr-remediation", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if selected == nil || selected.Number != 78 {
		t.Fatalf("selected = %+v, want 78 (77 is held by run-0 on the daemon)", selected)
	}
	number, ok, err := claimedPullRequestNumber(podRoot)
	if err != nil || !ok || number != 78 {
		t.Fatalf("claimedPullRequestNumber = %d, %v, %v", number, ok, err)
	}
	available, err := stageClaimAvailablePullRequests(podRoot, "run-1", []providers.PullRequestSummary{{Number: 77}, {Number: 78}, {Number: 79}}, time.Now())
	if err != nil || len(available) != 2 || available[0].Number != 78 || available[1].Number != 79 {
		t.Fatalf("claim-available PRs = %+v, %v; want 78 (ours) and 79 (free)", available, err)
	}
	if err := releasePRRemediationClaim(podRoot); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := claimedPullRequestNumber(podRoot); err != nil || ok {
		t.Fatalf("after release: claimed = %v, err = %v", ok, err)
	}
	reopened, err := localscheduler.OpenClaimLedger(filepath.Join(plane.layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if held := reopened.ForRunAll("run-0"); len(held) != 1 {
		t.Fatalf("run-0's claim on the daemon = %+v, want untouched", held)
	}
	if _, err := os.Stat(filepath.Join(podRoot, "scheduler")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the pod root grew a scheduler dir (err = %v); the plane must be the only ledger and log a pod touches", err)
	}
}
