//go:build cloudintegration

package claims

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/claimrelease"
	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/pgroles"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/localscheduler"
)

// planeFixture runs the plane and its lease manager as goobers_controller,
// the role production composes them on, so any write path missing from the
// grant inventory fails here. owner is the migrated database's owner session
// for fixture edits the controller is not granted.
type planeFixture struct {
	owner      *pgxpool.Pool
	controller *pgxpool.Pool
}

func newPlaneFixture(t *testing.T) planeFixture {
	t.Helper()
	owner := pgtest.Pool(t, "claimsplane")
	config := owner.Config()
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE "+pgx.Identifier{pgroles.Controller}.Sanitize())
		return err
	}
	controller, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(controller.Close)
	var role string
	if err := controller.QueryRow(t.Context(), `SELECT current_user`).Scan(&role); err != nil || role != pgroles.Controller {
		t.Fatalf("fixture session role = %q (%v), want %s: the grant check would be vacuous", role, err, pgroles.Controller)
	}
	return planeFixture{owner: owner, controller: controller}
}

// replica is one control process: its lease manager and its plane.
type replica struct {
	manager *cloudlease.Manager
	plane   *Plane
	handle  atomic.Pointer[cloudlease.Handle]
}

func (f planeFixture) replica(t *testing.T, holder string) *replica {
	t.Helper()
	r := &replica{manager: pgtest.Manager(t, f.controller, holder, 0)}
	plane, err := NewPlane(PlaneConfig{
		Pool: f.controller, InstanceID: pgtest.InstanceID, Lease: r.manager,
		Owner: r.handle.Load, DefaultLease: 30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.plane = plane
	return r
}

func (f planeFixture) leader(t *testing.T, holder string) *replica {
	t.Helper()
	r := f.replica(t, holder)
	r.handle.Store(pgtest.Acquire(t, r.manager))
	return r
}

func (f planeFixture) takeover(t *testing.T, holder string) *replica {
	t.Helper()
	r := f.replica(t, holder)
	r.handle.Store(pgtest.Takeover(t, f.owner, r.manager))
	return r
}

func request(item, run string) httpapi.ClaimRequest {
	return httpapi.ClaimRequest{Gaggle: "g", Provider: "github", ItemID: item, RunID: run, Workflow: "implementation"}
}

func planeCode(err error) string {
	var planeErr *httpapi.InterventionError
	if errors.As(err, &planeErr) {
		return fmt.Sprintf("%d %s", planeErr.Status, planeErr.Code)
	}
	return ""
}

func ownerEpoch(t *testing.T, f planeFixture, item string) int64 {
	t.Helper()
	var epoch int64
	if err := f.owner.QueryRow(t.Context(), `
		SELECT owner_epoch FROM control.claim WHERE instance_id=$1 AND external_id=$2`,
		pgtest.InstanceID, item).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	return epoch
}

func TestPlaneLifecycleAsController(t *testing.T) {
	f := newPlaneFixture(t)
	plane := f.leader(t, "a").plane
	ctx := t.Context()

	first, err := plane.Acquire(ctx, request("42", "run-a"))
	if err != nil || !first.Ok || first.ExpiresAt == nil {
		t.Fatalf("acquire = %+v, %v", first, err)
	}
	if refused, err := plane.Acquire(ctx, request("42", "run-b")); err != nil || refused.Ok || refused.Holder != "run-a" {
		t.Fatalf("contending acquire = %+v, %v; want refusal naming run-a", refused, err)
	}
	renewed, err := plane.Renew(ctx, request("42", "run-a"))
	if err != nil || !renewed.Ok || !renewed.ExpiresAt.After(*first.ExpiresAt) {
		t.Fatalf("renew = %+v, %v", renewed, err)
	}
	if _, err := plane.Acquire(ctx, httpapi.ClaimRequest{Gaggle: "g", Provider: "github", ItemID: "42", RunID: "run-a", Workflow: "other"}); planeCode(err) != "409 workflow_mismatch" {
		t.Fatalf("same-run acquire under another workflow = %v", err)
	}
	listed, err := plane.List(ctx, httpapi.ClaimListRequest{RunID: "run-a", Scope: httpapi.ClaimListScopeRun})
	if err != nil || len(listed.Entries) != 1 || listed.Entries[0].ItemID != "42" {
		t.Fatalf("run list = %+v, %v", listed, err)
	}

	verify := httpapi.ClaimVerificationRequest{
		RunID: "run-a", Gaggle: "g", Provider: "github", ItemID: "42", OwnerRunID: "run-a",
		ClaimedAt: listed.Entries[0].ClaimedAt,
		// Observed at the grant instant: on the database clock, never ahead of it.
		Observation: httpapi.ClaimVerification{State: "verified", ObservedAt: listed.Entries[0].ClaimedAt, ProviderRunID: "run-a"},
	}
	if recorded, err := plane.RecordVerification(ctx, verify); err != nil || !recorded.Ok {
		t.Fatalf("record verification = %+v, %v", recorded, err)
	}
	if again, err := plane.RecordVerification(ctx, verify); err != nil || again.Ok {
		t.Fatalf("replayed observation = %+v, %v; want Ok=false (not newer)", again, err)
	}

	settle := request("42", "run-a")
	settle.Outcome = "completed"
	if settled, err := plane.Settle(ctx, settle); err != nil || !settled.Ok {
		t.Fatalf("settle = %+v, %v", settled, err)
	}
	if again, err := plane.Settle(ctx, settle); err != nil || !again.Ok {
		t.Fatalf("retried settle = %+v, %v; want idempotent no-op", again, err)
	}
	history, err := plane.List(ctx, httpapi.ClaimListRequest{Gaggle: "g", Provider: "github", RunID: "run-a", Scope: httpapi.ClaimListScopeNamespace, IncludeHistory: true})
	if err != nil || len(history.Entries) != 0 || len(history.History) != 1 ||
		history.History[0].ReleaseReason.Disposition != claimrelease.Completed || history.History[0].ReleaseReason.Code != "settled" ||
		history.History[0].Verification.State != "verified" {
		t.Fatalf("namespace history = %+v, %v", history, err)
	}
	if next, err := plane.Acquire(ctx, request("42", "run-b")); err != nil || !next.Ok {
		t.Fatalf("acquire after settle = %+v, %v", next, err)
	}
}

func TestPlanePublicationCustody(t *testing.T) {
	f := newPlaneFixture(t)
	plane := f.leader(t, "a").plane
	ctx := t.Context()
	item := localscheduler.PublicationLockItemIDPrefix + "owner/repo/7"
	if got, err := plane.Acquire(ctx, request(item, "run-a")); err != nil || !got.Ok {
		t.Fatalf("publication acquire = %+v, %v", got, err)
	}
	if got, err := plane.Acquire(ctx, request(item, "run-a")); err != nil || got.Ok {
		t.Fatalf("same-run publication re-acquire = %+v, %v; want refusal", got, err)
	}
	if got, err := plane.Renew(ctx, request(item, "run-a")); err != nil || got.Ok {
		t.Fatalf("publication renew = %+v, %v; want Ok=false", got, err)
	}
	if got, err := plane.Release(ctx, httpapi.ClaimRequest{Gaggle: "g", Provider: "github", RunID: "run-a"}); err != nil || len(got.Released) != 0 {
		t.Fatalf("release-all = %+v, %v; publication locks are skipped", got, err)
	}
	release := request(item, "run-a")
	release.Reason = claimrelease.Completed.Because("done", "")
	if _, err := plane.Release(ctx, release); planeCode(err) != "409 publication_custody" {
		t.Fatalf("unverified publication release = %v", err)
	}
	release.Reason.Code = localscheduler.PublicationLockReleaseCode
	if got, err := plane.Release(ctx, release); err != nil || !got.Ok {
		t.Fatalf("verified publication release = %+v, %v", got, err)
	}
	if got, err := plane.Acquire(ctx, request(item, "run-b")); err != nil || !got.Ok {
		t.Fatalf("acquire after verified release = %+v, %v", got, err)
	}
}

func TestPlaneReleaseAllReportsEachRelease(t *testing.T) {
	f := newPlaneFixture(t)
	plane := f.leader(t, "a").plane
	ctx := t.Context()
	for _, item := range []string{"1", "2"} {
		if _, err := plane.Acquire(ctx, request(item, "run-a")); err != nil {
			t.Fatal(err)
		}
	}
	other := request("3", "run-a")
	other.Provider = "gitea"
	if _, err := plane.Acquire(ctx, other); err != nil {
		t.Fatal(err)
	}
	got, err := plane.Release(ctx, httpapi.ClaimRequest{Gaggle: "g", Provider: "github", RunID: "run-a", Reason: claimrelease.NoWork.Reason()})
	if err != nil || !got.Ok || len(got.Released) != 2 {
		t.Fatalf("namespace release-all = %+v, %v; want items 1 and 2", got, err)
	}
	listed, err := plane.List(ctx, httpapi.ClaimListRequest{RunID: "run-a", Scope: httpapi.ClaimListScopeRun})
	if err != nil || len(listed.Entries) != 1 || listed.Entries[0].Provider != "gitea" {
		t.Fatalf("remaining = %+v, %v", listed, err)
	}
}

func TestPlaneRecoverReleasesExpiredAndTerminalLeases(t *testing.T) {
	f := newPlaneFixture(t)
	plane := f.leader(t, "a").plane
	ctx := t.Context()
	for item, run := range map[string]string{"expired": "run-live", "terminal": "run-done", "live": "run-live"} {
		if _, err := plane.Acquire(ctx, request(item, run)); err != nil {
			t.Fatal(err)
		}
	}
	publication := localscheduler.PublicationLockItemIDPrefix + "owner/repo/9"
	if _, err := plane.Acquire(ctx, request(publication, "run-done")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.Exec(ctx, `UPDATE control.claim SET expires_at=clock_timestamp()-interval '1 second'
		WHERE instance_id=$1 AND external_id='expired'`, pgtest.InstanceID); err != nil {
		t.Fatal(err)
	}
	finishRun(t, f, "run-done", `{"schema":"goobers.dev/journal/event/v1","seq":1,"type":"run.finished","branch":0,"time":"2026-10-23T00:00:00Z","status":"failed"}`)

	got, err := plane.Recover(ctx, httpapi.ClaimRecoverRequest{RunID: "run-live"})
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]claimrelease.Disposition{}
	for _, entry := range got.Released {
		reasons[entry.ItemID] = ""
	}
	if len(got.Released) != 2 {
		t.Fatalf("recovered = %+v, want the expired and the terminal lease", got.Released)
	}
	history, err := plane.List(ctx, httpapi.ClaimListRequest{Gaggle: "g", Provider: "github", RunID: "run-live", Scope: httpapi.ClaimListScopeNamespace, IncludeHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range history.History {
		reasons[entry.ItemID] = entry.ReleaseReason.Disposition
	}
	if reasons["expired"] != claimrelease.StallSwept || reasons["terminal"] != claimrelease.Failed {
		t.Fatalf("release reasons = %v, want stall-swept and the run's own failed outcome", reasons)
	}
	remaining := map[string]bool{}
	for _, entry := range history.Entries {
		remaining[entry.ItemID] = true
	}
	if !remaining["live"] || !remaining[publication] || len(remaining) != 2 {
		t.Fatalf("remaining = %v, want the live lease and the terminal run's publication lock", remaining)
	}
}

func finishRun(t *testing.T, f planeFixture, runID, finished string) {
	t.Helper()
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO journal.run (instance_id,run_id,gaggle,workflow,run_yaml,created_at)
		  VALUES ($1,$2,'g','implementation','',clock_timestamp())`, []any{pgtest.InstanceID, runID}},
		{`INSERT INTO journal.run_event (instance_id,run_id,seq,type,at,body)
		  VALUES ($1,$2,1,'run.finished',clock_timestamp(),$3)`, []any{pgtest.InstanceID, runID, []byte(finished)}},
		{`INSERT INTO journal.run_checkpoint (instance_id,run_id,phase,terminal,state,at_seq)
		  VALUES ($1,$2,'failed',true,'',1)`, []any{pgtest.InstanceID, runID}},
	} {
		if _, err := f.owner.Exec(t.Context(), statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPlaneContainsPodsAndRefusesUnportedFeatures(t *testing.T) {
	f := newPlaneFixture(t)
	plane := f.leader(t, "a").plane
	ctx := t.Context()
	foreign := httpapi.ClaimListRequest{Gaggle: "g", Provider: "github", RunID: "run-x", Scope: httpapi.ClaimListScopeNamespace, PodScoped: true}
	if _, err := plane.List(ctx, foreign); planeCode(err) != "403 gaggle_mismatch" {
		t.Fatalf("pod namespace list for an unknown run = %v", err)
	}
	finishRun(t, f, "run-x", `{"schema":"goobers.dev/journal/event/v1","seq":1,"type":"run.finished","branch":0,"time":"2026-10-23T00:00:00Z","status":"failed"}`)
	if _, err := plane.List(ctx, foreign); err != nil {
		t.Fatalf("pod namespace list for its own gaggle = %v", err)
	}
	cleanup := request("42", "run-x")
	cleanup.ProviderCleanup, cleanup.ClaimedAt = true, time.Now()
	if _, err := plane.Release(ctx, cleanup); planeCode(err) != fmt.Sprintf("%d claims_provider_cleanup_unavailable", http.StatusServiceUnavailable) {
		t.Fatalf("provider-cleanup release = %v", err)
	}
	snapshot, err := plane.List(ctx, httpapi.ClaimListRequest{RunID: "run-x", Scope: httpapi.ClaimListScopeRun, IncludeHistory: true, Execution: true})
	if err != nil || snapshot.ClaimVisibility != "local" || snapshot.ObservedAt.IsZero() {
		t.Fatalf("execution snapshot = %+v, %v", snapshot, err)
	}
	if _, err := plane.Acquire(ctx, httpapi.ClaimRequest{Gaggle: "legacy", Provider: "legacy", ItemID: "1", RunID: "r", Workflow: "w"}); planeCode(err) != "400 invalid_request" {
		t.Fatalf("legacy namespace acquire = %v", err)
	}
}

// TestPlaneFailoverAdoptsAcrossTwoTakeovers proves a pod's lease outlives two
// consecutive controller takeovers: the third epoch renews and releases the
// first epoch's row, while every stale replica answers retryable 503s and
// changes nothing.
func TestPlaneFailoverAdoptsAcrossTwoTakeovers(t *testing.T) {
	f := newPlaneFixture(t)
	first := f.leader(t, "a")
	ctx := t.Context()
	if got, err := first.plane.Acquire(ctx, request("42", "run-a")); err != nil || !got.Ok {
		t.Fatalf("acquire = %+v, %v", got, err)
	}
	second := f.takeover(t, "b")
	third := f.takeover(t, "c")
	if epoch := ownerEpoch(t, f, "42"); epoch != first.handle.Load().Epoch {
		t.Fatalf("row epoch = %d before adoption", epoch)
	}
	for name, stale := range map[string]*replica{"first": first, "second": second} {
		if _, err := stale.plane.Renew(ctx, request("42", "run-a")); planeCode(err) != "503 recovering" {
			t.Fatalf("%s replica renew = %v, want retryable 503 recovering", name, err)
		}
		if _, err := stale.plane.Release(ctx, request("42", "run-a")); planeCode(err) != "503 recovering" {
			t.Fatalf("%s replica release = %v, want retryable 503 recovering", name, err)
		}
	}
	got, err := third.plane.Renew(ctx, request("42", "run-a"))
	if err != nil || !got.Ok {
		t.Fatalf("renew after two takeovers = %+v, %v", got, err)
	}
	if epoch := ownerEpoch(t, f, "42"); epoch != third.handle.Load().Epoch {
		t.Fatalf("row epoch after renew = %d, want adopted into %d", epoch, third.handle.Load().Epoch)
	}
	if refused, err := third.plane.Acquire(ctx, request("42", "run-b")); err != nil || refused.Ok || refused.Holder != "run-a" {
		t.Fatalf("contender after failover = %+v, %v", refused, err)
	}
	if released, err := third.plane.Release(ctx, request("42", "run-a")); err != nil || !released.Ok {
		t.Fatalf("release after failover = %+v, %v", released, err)
	}
}

// TestPlaneTwoReplicaConcurrency races contenders for one item through two
// replicas, across a takeover: exactly one run ever holds the item, the
// standby or stale replica only answers retryable 503s, and nothing 500s.
func TestPlaneTwoReplicaConcurrency(t *testing.T) {
	f := newPlaneFixture(t)
	active := f.leader(t, "a")
	standby := f.replica(t, "b")
	ctx := t.Context()
	const contenders = 16
	race := func(phase string, replicas [2]*replica) map[string]bool {
		var wg sync.WaitGroup
		var mu sync.Mutex
		winners := map[string]bool{}
		for i := range contenders {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				run := fmt.Sprintf("%s-run-%d", phase, i)
				got, err := replicas[i%2].plane.Acquire(context.WithoutCancel(ctx), request("contended", run))
				if err != nil && planeCode(err) != "503 recovering" {
					t.Errorf("%s acquire = %v", run, err)
					return
				}
				if err == nil && got.Ok {
					mu.Lock()
					winners[run] = true
					mu.Unlock()
				}
			}(i)
		}
		wg.Wait()
		return winners
	}
	before := race("before", [2]*replica{active, standby})
	if len(before) != 1 {
		t.Fatalf("winners before takeover = %v, want exactly one", before)
	}
	var winner string
	for run := range before {
		winner = run
	}
	standby.handle.Store(pgtest.Takeover(t, f.owner, standby.manager))
	if after := race("after", [2]*replica{active, standby}); len(after) != 0 {
		t.Fatalf("winners after takeover = %v, want none while %s holds the lease", after, winner)
	}
	if got, err := standby.plane.Renew(ctx, request("contended", winner)); err != nil || !got.Ok {
		t.Fatalf("winner renew on the new leader = %+v, %v", got, err)
	}
}

// TestPlaneEpochMaintenanceAcrossTwoTakeovers drives the epoch services'
// takeover sequence on the third epoch after two consecutive takeovers:
// adopt every older epoch, rebuild renewals for open runs, then reap expiry
// only once the recovery gate opens.
func TestPlaneEpochMaintenanceAcrossTwoTakeovers(t *testing.T) {
	f := newPlaneFixture(t)
	first := f.leader(t, "a")
	ctx := t.Context()
	publication := localscheduler.PublicationLockItemIDPrefix + "owner/repo/1"
	for item, run := range map[string]string{"live": "run-open", "lapsed": "run-open", "closed": "run-closed", publication: "run-open"} {
		if got, err := first.plane.Acquire(ctx, request(item, run)); err != nil || !got.Ok {
			t.Fatalf("acquire %s = %+v, %v", item, got, err)
		}
	}
	expire := func(item string) {
		t.Helper()
		if _, err := f.owner.Exec(ctx, `UPDATE control.claim SET expires_at=clock_timestamp()-interval '1 second'
			WHERE instance_id=$1 AND external_id=$2`, pgtest.InstanceID, item); err != nil {
			t.Fatal(err)
		}
	}
	expire("lapsed")
	second := f.takeover(t, "b")
	third := f.takeover(t, "c")
	handle := third.handle.Load()

	if adopted, err := third.plane.AdoptAll(ctx, handle); err != nil || adopted != 4 {
		t.Fatalf("adopt = %d, %v; want all 4 epoch-1 rows", adopted, err)
	}
	for _, item := range []string{"live", "lapsed", "closed", publication} {
		if epoch := ownerEpoch(t, f, item); epoch != handle.Epoch {
			t.Fatalf("%s epoch = %d, want %d", item, epoch, handle.Epoch)
		}
	}
	if adopted, err := third.plane.AdoptAll(ctx, handle); err != nil || adopted != 0 {
		t.Fatalf("second adopt = %d, %v; want idempotent", adopted, err)
	}
	for name, stale := range map[string]*replica{"first": first, "second": second} {
		if _, err := stale.plane.AdoptAll(ctx, stale.handle.Load()); planeCode(err) != "503 recovering" {
			t.Fatalf("%s replica adopt = %v, want retryable 503 recovering", name, err)
		}
	}

	gate := localscheduler.NewRecoveryGate()
	for name, closed := range map[string]RecoveryGate{"closed": gate, "nil": nil} {
		if released, err := third.plane.RecoverExpired(ctx, handle, closed); err != nil || released != nil {
			t.Fatalf("%s gate recover = %+v, %v; want a no-op", name, released, err)
		}
	}

	var claimedBefore time.Time
	if err := f.owner.QueryRow(ctx, `SELECT claimed_at FROM control.claim WHERE instance_id=$1 AND external_id='lapsed'`,
		pgtest.InstanceID).Scan(&claimedBefore); err != nil {
		t.Fatal(err)
	}
	renewed, err := third.plane.RenewRuns(ctx, handle, map[string]bool{"run-open": true})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, entry := range renewed {
		got[entry.ItemID] = true
	}
	if len(got) != 2 || !got["live"] || !got["lapsed"] {
		t.Fatalf("renewed = %v, want the open run's live and lapsed leases, never its publication lock or a closed run", got)
	}
	listed, err := third.plane.List(ctx, httpapi.ClaimListRequest{RunID: "run-open", Scope: httpapi.ClaimListScopeRun})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range listed.Entries {
		if entry.ItemID == "lapsed" && (!entry.ExpiresAt.After(time.Now().Add(-time.Minute)) || !entry.ClaimedAt.After(claimedBefore)) {
			t.Fatalf("lapsed lease after rebuild = %+v, want a fresh grant", entry)
		}
	}

	expire("closed")
	gate.MarkRenewalRebuilt()
	released, err := third.plane.RecoverExpired(ctx, handle, gate)
	if err != nil || len(released) != 1 || released[0].ItemID != "closed" {
		t.Fatalf("recover after rebuild = %+v, %v; want only the closed run's expired lease", released, err)
	}
	history, err := third.plane.List(ctx, httpapi.ClaimListRequest{RunID: "run-closed", Scope: httpapi.ClaimListScopeRun, IncludeHistory: true})
	if err != nil || len(history.History) != 1 || history.History[0].ReleaseReason.Disposition != claimrelease.StallSwept {
		t.Fatalf("closed run history = %+v, %v", history, err)
	}
}
