//go:build cloudintegration

package claims

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
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
	everyRunClosed := func(context.Context, string) (bool, error) { return true, nil }
	for name, closed := range map[string]RecoveryGate{"closed": gate, "nil": nil} {
		if released, err := third.plane.RecoverExpired(ctx, handle, closed, everyRunClosed); err != nil || released != nil {
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
	expire("live")
	gate.MarkRenewalRebuilt()
	if released, err := third.plane.RecoverExpired(ctx, handle, gate, nil); err != nil || released != nil {
		t.Fatalf("recover without a liveness confirmation = %+v, %v; want a no-op", released, err)
	}
	unknown := func(context.Context, string) (bool, error) { return true, errors.New("describe failed") }
	if released, err := third.plane.RecoverExpired(ctx, handle, gate, unknown); err != nil || len(released) != 0 {
		t.Fatalf("recover with an unconfirmed liveness = %+v, %v; want every claim kept", released, err)
	}
	confirmed := func(_ context.Context, runID string) (bool, error) { return runID == "run-closed", nil }
	released, err := third.plane.RecoverExpired(ctx, handle, gate, confirmed)
	if err != nil || len(released) != 1 || released[0].ItemID != "closed" {
		t.Fatalf("recover after rebuild = %+v, %v; want only the confirmed-closed run's expired lease", released, err)
	}
	// Renew before reap in one tick revives the open run's expired lease, as
	// the ledger's RenewRuns does, so even an everything-closed confirmation
	// cannot reap it afterwards.
	if revived, err := third.plane.RenewRuns(ctx, handle, map[string]bool{"run-open": true}); err != nil || len(revived) != 2 {
		t.Fatalf("renew before reap = %+v, %v", revived, err)
	}
	if reaped, err := third.plane.RecoverExpired(ctx, handle, gate, everyRunClosed); err != nil || len(reaped) != 0 {
		t.Fatalf("reap after renewal = %+v, %v; want the revived leases kept", reaped, err)
	}
	history, err := third.plane.List(ctx, httpapi.ClaimListRequest{RunID: "run-closed", Scope: httpapi.ClaimListScopeRun, IncludeHistory: true})
	if err != nil || len(history.History) != 1 || history.History[0].ReleaseReason.Disposition != claimrelease.StallSwept {
		t.Fatalf("closed run history = %+v, %v", history, err)
	}
}

// TestPlaneRecoverExpiredRefusesTypedNilGate: a typed nil *RecoveryGate is
// non-nil as an interface and its nil-safe method permits, so it must be
// treated as closed or an expired live-run lease is reaped before rebuild.
func TestPlaneRecoverExpiredRefusesTypedNilGate(t *testing.T) {
	f := newPlaneFixture(t)
	r := f.leader(t, "a")
	ctx := t.Context()
	if got, err := r.plane.Acquire(ctx, request("held", "live-run")); err != nil || !got.Ok {
		t.Fatalf("acquire = %+v, %v", got, err)
	}
	if _, err := f.owner.Exec(ctx, `UPDATE control.claim SET expires_at=clock_timestamp()-interval '1 second'
		WHERE instance_id=$1`, pgtest.InstanceID); err != nil {
		t.Fatal(err)
	}
	var gate *localscheduler.RecoveryGate
	if !gate.RecoveryPermitted() {
		t.Fatal("premise: the local nil gate permits recovery")
	}
	released, err := r.plane.RecoverExpired(ctx, r.handle.Load(), gate,
		func(context.Context, string) (bool, error) { return true, nil })
	if err != nil || len(released) != 0 {
		t.Fatalf("typed nil gate recover = %+v, %v; want a no-op", released, err)
	}
	var remaining int
	if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM control.claim WHERE instance_id=$1`, pgtest.InstanceID).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("remaining leases = %d, %v; want the expired live-run lease kept", remaining, err)
	}
}

// historyShape is the part of a history record both ledgers must agree on.
type historyShape struct {
	Run, Item string
	Released  bool
	Reason    claimrelease.Disposition
}

func shapes(entries []httpapi.ClaimEntry) []historyShape {
	out := make([]historyShape, 0, len(entries))
	for _, entry := range entries {
		out = append(out, historyShape{entry.RunID, entry.ItemID, entry.ReleasedAt != nil, entry.ReleaseReason.Disposition})
	}
	return out
}

func localShapes(entries []localscheduler.ClaimEntry) []historyShape {
	out := make([]historyShape, 0, len(entries))
	for _, entry := range entries {
		out = append(out, historyShape{entry.RunID, entry.ItemID, entry.ReleasedAt != nil, entry.ReleaseReason.Disposition})
	}
	return out
}

// TestPlaneHistoryMatchesFileLedger drives the PG plane and a file
// ClaimLedger through the same grants and releases and requires the same
// history: one record per run and key, present while live, completed on
// release, superseded by a re-grant (ClaimLedger.storeClaim/recordHistory,
// HistoryForRun, HistorySnapshot).
func TestPlaneHistoryMatchesFileLedger(t *testing.T) {
	f := newPlaneFixture(t)
	p := f.leader(t, "a").plane
	ctx := t.Context()
	local, err := localscheduler.OpenClaimLedger(filepath.Join(t.TempDir(), "claims.json"))
	if err != nil {
		t.Fatal(err)
	}
	grant := func(item, run string) {
		t.Helper()
		key := localscheduler.ClaimKey{Gaggle: "g", Provider: "github", ExternalID: item}
		if ok, _, err := local.ClaimScoped(key, run, "implementation", time.Hour); err != nil || !ok {
			t.Fatalf("local grant %s/%s = %v, %v", item, run, ok, err)
		}
		if got, err := p.Acquire(ctx, request(item, run)); err != nil || !got.Ok {
			t.Fatalf("plane grant %s/%s = %+v, %v", item, run, got, err)
		}
	}
	release := func(item, run string) {
		t.Helper()
		reason := claimrelease.Completed.Reason()
		key := localscheduler.ClaimKey{Gaggle: "g", Provider: "github", ExternalID: item}
		if err := local.ReleaseScoped(key, run, reason); err != nil {
			t.Fatal(err)
		}
		req := request(item, run)
		req.Reason = reason
		if _, err := p.Release(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	compareRun := func(step, run string) {
		t.Helper()
		got, err := p.List(ctx, httpapi.ClaimListRequest{RunID: run, Scope: httpapi.ClaimListScopeRun, IncludeHistory: true})
		if err != nil {
			t.Fatal(err)
		}
		if pg, file := shapes(got.History), localShapes(local.HistoryForRun(run)); !reflect.DeepEqual(pg, file) {
			t.Fatalf("%s: %s history PG=%+v file=%+v", step, run, pg, file)
		}
	}
	grant("42", "run-a")
	compareRun("live", "run-a")
	release("42", "run-a")
	compareRun("released", "run-a")
	grant("42", "run-a")
	compareRun("re-granted", "run-a")
	release("42", "run-a")
	compareRun("released twice", "run-a")
	grant("7", "run-a")
	grant("42", "run-b")
	compareRun("second key", "run-a")
	compareRun("other run", "run-b")

	namespace, err := p.List(ctx, httpapi.ClaimListRequest{Gaggle: "g", Provider: "github", RunID: "run-a", Scope: httpapi.ClaimListScopeNamespace, IncludeHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	if pg, file := shapes(namespace.History), localShapes(local.HistorySnapshot()); !reflect.DeepEqual(pg, file) {
		t.Fatalf("namespace history PG=%+v file=%+v", pg, file)
	}
}

// TestPlaneExpiredOverwriteKeepsOwnerHistory: overwriting an expired lease
// leaves its owner's record, unreleased, as the file ledger does.
func TestPlaneExpiredOverwriteKeepsOwnerHistory(t *testing.T) {
	f := newPlaneFixture(t)
	p := f.leader(t, "a").plane
	ctx := t.Context()
	now := time.Now()
	local, err := localscheduler.OpenClaimLedger(filepath.Join(t.TempDir(), "claims.json"),
		localscheduler.WithLedgerClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	key := localscheduler.ClaimKey{Gaggle: "g", Provider: "github", ExternalID: "42"}
	if ok, _, err := local.ClaimScoped(key, "old", "implementation", time.Second); err != nil || !ok {
		t.Fatal(err)
	}
	if got, err := p.Acquire(ctx, request("42", "old")); err != nil || !got.Ok {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, err := f.owner.Exec(ctx, `UPDATE control.claim SET expires_at=clock_timestamp()-interval '1 second'
		WHERE instance_id=$1`, pgtest.InstanceID); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := local.ClaimScoped(key, "new", "implementation", time.Hour); err != nil || !ok {
		t.Fatal(err)
	}
	if got, err := p.Acquire(ctx, request("42", "new")); err != nil || !got.Ok {
		t.Fatal(err)
	}
	got, err := p.List(ctx, httpapi.ClaimListRequest{RunID: "old", Scope: httpapi.ClaimListScopeRun, IncludeHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	if pg, file := shapes(got.History), localShapes(local.HistoryForRun("old")); !reflect.DeepEqual(pg, file) || len(pg) != 1 {
		t.Fatalf("overwritten owner history PG=%+v file=%+v; want its one unreleased record", pg, file)
	}
}

// commitReplyLoss drops the first successful COMMIT reply after PostgreSQL
// has committed, the protocol boundary where the outcome becomes unknown.
type commitReplyLoss struct {
	net.Conn
	armed, dropped *atomic.Bool
}

func (c *commitReplyLoss) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 && bytes.Contains(p[:n], []byte("COMMIT\x00")) && c.armed.CompareAndSwap(true, false) {
		c.dropped.Store(true)
		return 0, io.ErrUnexpectedEOF
	}
	return n, err
}

// TestPlaneLostCommitIsAmbiguous: a write that committed but lost its reply
// must not be advertised as safely replayable (503 recovering); it is an
// ambiguous budget overrun.
func TestPlaneLostCommitIsAmbiguous(t *testing.T) {
	f := newPlaneFixture(t)
	config := f.controller.Config()
	dial := config.ConnConfig.DialFunc
	var armed, dropped atomic.Bool
	config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &commitReplyLoss{Conn: conn, armed: &armed, dropped: &dropped}, nil
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	manager := pgtest.Manager(t, pool, "lost-commit", 0)
	handle := pgtest.Acquire(t, manager)
	plane, err := NewPlane(PlaneConfig{Pool: pool, InstanceID: pgtest.InstanceID, Lease: manager,
		Owner: func() *cloudlease.Handle { return handle }, DefaultLease: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	_, writeErr := plane.Acquire(t.Context(), request("committed", "run-a"))
	if !dropped.Load() {
		t.Fatalf("premise: no COMMIT reply was dropped: %v", writeErr)
	}
	var count int
	if err := f.owner.QueryRow(t.Context(), `SELECT count(*) FROM control.claim WHERE instance_id=$1 AND external_id='committed'`,
		pgtest.InstanceID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("premise: the server committed, count=%d (%v), write error %v", count, err, writeErr)
	}
	if !errors.Is(writeErr, cloudlease.ErrCommitUnknown) || planeCode(writeErr) != "503 request_budget_exceeded" {
		t.Fatalf("committed write answered %q (%v), want the ambiguous 503 request_budget_exceeded", planeCode(writeErr), writeErr)
	}
}

// TestPlaneHistoryRetentionMatchesFileLedger: retention is per run, as
// ClaimLedger.retainedHistory keeps it. A run holding any lease keeps every
// record however old; a run holding nothing keeps them only while its newest
// activity is within ClaimHistoryTTL, and the next claim prunes them.
func TestPlaneHistoryRetentionMatchesFileLedger(t *testing.T) {
	f := newPlaneFixture(t)
	p := f.leader(t, "a").plane
	ctx := t.Context()
	for _, step := range []struct{ item, run string }{{"old", "active"}, {"old", "quiet"}, {"recent", "recent"}} {
		if _, err := p.Acquire(ctx, request(step.item+"-"+step.run, step.run)); err != nil {
			t.Fatal(err)
		}
		release := request(step.item+"-"+step.run, step.run)
		release.Reason = claimrelease.Completed.Reason()
		if _, err := p.Release(ctx, release); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.Acquire(ctx, request("held", "active")); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"claim_history", "claim_run_history"} {
		if _, err := f.owner.Exec(ctx, `
			UPDATE control.`+table+`
			SET claimed_at=claimed_at-$2::interval, renewed_at=renewed_at-$2::interval, released_at=released_at-$2::interval
			WHERE instance_id=$1 AND external_id LIKE 'old-%'`,
			pgtest.InstanceID, localscheduler.ClaimHistoryTTL+time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	items := func(run string) map[string]bool {
		t.Helper()
		got, err := p.List(ctx, httpapi.ClaimListRequest{RunID: run, Scope: httpapi.ClaimListScopeRun, IncludeHistory: true})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, entry := range got.History {
			out[entry.ItemID] = true
		}
		return out
	}
	if got := items("active"); !got["old-active"] || !got["held"] {
		t.Fatalf("active run history = %v, want its expired-TTL record kept while it holds a lease", got)
	}
	if got := items("quiet"); len(got) != 0 {
		t.Fatalf("quiet run history = %v, want nothing past the TTL", got)
	}
	if got := items("recent"); !got["recent-recent"] {
		t.Fatalf("recent run history = %v, want its record within the TTL", got)
	}
	if _, err := p.Acquire(ctx, request("trigger-prune", "other")); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"claim_history", "claim_run_history"} {
		var stored int
		if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM control.`+table+` WHERE instance_id=$1 AND run_id='quiet'`,
			pgtest.InstanceID).Scan(&stored); err != nil || stored != 0 {
			t.Fatalf("quiet run %s rows after prune = %d (%v), want 0", table, stored, err)
		}
	}
	var kept int
	if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM control.claim_run_history WHERE instance_id=$1 AND run_id='active'`,
		pgtest.InstanceID).Scan(&kept); err != nil || kept != 2 {
		t.Fatalf("active run records after prune = %d (%v), want both kept", kept, err)
	}
}

// TestPlaneReleaseRunIsTheTerminalRelease: the run-wide terminal release
// (ClaimLifecycle via ReleaseAllForRun) returns what it released, skips
// publication locks, is idempotent, and refuses a stale handle retryably.
func TestPlaneReleaseRunIsTheTerminalRelease(t *testing.T) {
	f := newPlaneFixture(t)
	first := f.leader(t, "a")
	ctx := t.Context()
	publication := localscheduler.PublicationLockItemIDPrefix + "owner/repo/3"
	for _, item := range []string{"1", "2", publication} {
		if got, err := first.plane.Acquire(ctx, request(item, "run-done")); err != nil || !got.Ok {
			t.Fatalf("acquire %s = %+v, %v", item, got, err)
		}
	}
	if _, err := first.plane.Acquire(ctx, request("other", "run-live")); err != nil {
		t.Fatal(err)
	}
	second := f.takeover(t, "b")
	if _, err := first.plane.ReleaseRun(ctx, first.handle.Load(), "run-done", claimrelease.Failed.Reason()); planeCode(err) != "503 recovering" {
		t.Fatalf("stale handle release = %v, want retryable 503 recovering", err)
	}
	released, err := second.plane.ReleaseRun(ctx, second.handle.Load(), "run-done", claimrelease.Failed.Because("stage_failed", ""))
	if err != nil || len(released) != 2 {
		t.Fatalf("release run = %+v, %v; want items 1 and 2", released, err)
	}
	if again, err := second.plane.ReleaseRun(ctx, second.handle.Load(), "run-done", claimrelease.Failed.Reason()); err != nil || len(again) != 0 {
		t.Fatalf("second release = %+v, %v; want none", again, err)
	}
	listed, err := second.plane.List(ctx, httpapi.ClaimListRequest{RunID: "run-done", Scope: httpapi.ClaimListScopeRun, IncludeHistory: true})
	if err != nil || len(listed.Entries) != 1 || listed.Entries[0].ItemID != publication {
		t.Fatalf("remaining = %+v, %v; want only the publication lock", listed.Entries, err)
	}
	for _, entry := range listed.History {
		if entry.ItemID != publication && (entry.ReleaseReason.Disposition != claimrelease.Failed || entry.ReleaseReason.Code != "stage_failed") {
			t.Fatalf("history %s reason = %+v", entry.ItemID, entry.ReleaseReason)
		}
	}
	if other, err := second.plane.List(ctx, httpapi.ClaimListRequest{RunID: "run-live", Scope: httpapi.ClaimListScopeRun}); err != nil || len(other.Entries) != 1 {
		t.Fatalf("other run = %+v, %v; want untouched", other, err)
	}
}

// TestPlaneHistorySeesPredecessorRelease: during a rolling upgrade the
// previous binary releases by appending to control.claim_history only. The
// current plane's history must still show that run's record released.
func TestPlaneHistorySeesPredecessorRelease(t *testing.T) {
	f := newPlaneFixture(t)
	r := f.leader(t, "a")
	ctx := t.Context()
	if got, err := r.plane.Acquire(ctx, request("42", "run-a")); err != nil || !got.Ok {
		t.Fatalf("acquire = %+v, %v", got, err)
	}
	// The previous claims.Store.ReleaseScoped, verbatim, as the controller.
	tag, err := f.controller.Exec(ctx, `
		WITH moved AS (
			DELETE FROM control.claim
			WHERE instance_id=$1 AND gaggle=$2 AND provider=$3 AND external_id=$4
			  AND run_id=$5 AND owner_epoch=$6
			RETURNING *
		)
		INSERT INTO control.claim_history
			(instance_id,gaggle,provider,external_id,run_id,workflow,owner_token,owner_epoch,
			 claimed_at,renewed_at,expires_at,released_at,release_reason,verification)
		SELECT instance_id,gaggle,provider,external_id,run_id,workflow,owner_token,owner_epoch,
		       claimed_at,renewed_at,expires_at,clock_timestamp(),$7,verification
		FROM moved`,
		pgtest.InstanceID, "g", "github", "42", "run-a", r.handle.Load().Epoch, []byte(`{"disposition":"abandoned"}`))
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("predecessor release = %v, %v", tag, err)
	}
	got, err := r.plane.List(ctx, httpapi.ClaimListRequest{RunID: "run-a", Scope: httpapi.ClaimListScopeRun, IncludeHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 0 || len(got.History) != 1 || got.History[0].ReleasedAt == nil ||
		got.History[0].ReleaseReason.Disposition != claimrelease.Abandoned {
		t.Fatalf("history after a predecessor release = %+v, want one released record", got)
	}
}

// TestPlaneMixedVersionRetentionKeepsRunHistory (reviewer probe E): after the
// previous binary releases one of a long-running run's leases, an unrelated
// claim's prune must keep the run's other records, because its newest
// activity across both history stores is that recent release.
func TestPlaneMixedVersionRetentionKeepsRunHistory(t *testing.T) {
	f := newPlaneFixture(t)
	r := f.leader(t, "review")
	ctx := t.Context()
	for _, item := range []string{"overwritten", "held"} {
		if got, err := r.plane.Acquire(ctx, request(item, "long-run")); err != nil || !got.Ok {
			t.Fatalf("grant: %+v %v", got, err)
		}
	}
	// Exercise the real expired-overwrite path; only timestamps are fixtures.
	if _, err := f.owner.Exec(ctx, `UPDATE control.claim SET expires_at=now()-interval '1 second' WHERE external_id='overwritten'`); err != nil {
		t.Fatal(err)
	}
	if got, err := r.plane.Acquire(ctx, request("overwritten", "new-holder")); err != nil || !got.Ok {
		t.Fatalf("overwrite: %+v %v", got, err)
	}
	if _, err := f.owner.Exec(ctx, `UPDATE control.claim_run_history SET claimed_at=now()-interval '60 days',renewed_at=now()-interval '60 days' WHERE run_id='long-run';
 UPDATE control.claim SET claimed_at=now()-interval '60 days',renewed_at=now()-interval '60 days' WHERE run_id='long-run'`); err != nil {
		t.Fatal(err)
	}
	// Previous Store's release, with its production role and current live epoch.
	tag, err := f.controller.Exec(ctx, `WITH moved AS (
 DELETE FROM control.claim WHERE instance_id=$1 AND gaggle=$2 AND provider=$3 AND external_id=$4 AND run_id=$5 AND owner_epoch=$6 RETURNING *)
 INSERT INTO control.claim_history (instance_id,gaggle,provider,external_id,run_id,workflow,owner_token,owner_epoch,claimed_at,renewed_at,expires_at,released_at,release_reason,verification)
 SELECT instance_id,gaggle,provider,external_id,run_id,workflow,owner_token,owner_epoch,claimed_at,renewed_at,expires_at,clock_timestamp(),$7,verification FROM moved`, pgtest.InstanceID, "g", "github", "held", "long-run", r.handle.Load().Epoch, []byte(`{"disposition":"completed"}`))
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("N-1 release: %v %v", tag, err)
	}
	count := func() int {
		got, err := r.plane.List(ctx, httpapi.ClaimListRequest{RunID: "long-run", Scope: httpapi.ClaimListScopeRun, IncludeHistory: true})
		if err != nil {
			t.Fatal(err)
		}
		return len(got.History)
	}
	if n := count(); n != 2 {
		t.Fatalf("before prune %d, want2", n)
	}
	if got, err := r.plane.Acquire(ctx, request("trigger", "unrelated")); err != nil || !got.Ok {
		t.Fatalf("trigger: %+v %v", got, err)
	}
	if n := count(); n != 2 {
		t.Fatalf("mixed-version prune lost retained history: before=2 after=%d; release just now must retain whole run for %v", n, localscheduler.ClaimHistoryTTL)
	}
}

// TestPlaneDualWriteIsAtomicAtOneInstant (reviewer probe): a release that
// fails in the run-history write rolls back the claim deletion and the
// incarnation append, and a successful one stamps both at one instant; no
// other runtime role can touch run history.
func TestPlaneDualWriteIsAtomicAtOneInstant(t *testing.T) {
	f := newPlaneFixture(t)
	r := f.leader(t, "review")
	ctx := t.Context()
	req := request("atomic", "run")
	req.Reason = claimrelease.Completed.Reason()
	if got, err := r.plane.Acquire(ctx, req); err != nil || !got.Ok {
		t.Fatalf("grant: %+v %v", got, err)
	}
	if _, err := f.owner.Exec(ctx, `ALTER TABLE control.claim_run_history ADD CONSTRAINT review_reject_release CHECK(released_at IS NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.plane.Release(ctx, req); err == nil {
		t.Fatal("injected release rejection ignored")
	}
	var active, old int
	if err := f.owner.QueryRow(ctx, `SELECT (SELECT count(*) FROM control.claim WHERE external_id='atomic'),(SELECT count(*) FROM control.claim_history WHERE external_id='atomic')`).Scan(&active, &old); err != nil {
		t.Fatal(err)
	}
	if active != 1 || old != 0 {
		t.Fatalf("non-atomic rollback: active=%d old=%d", active, old)
	}
	if _, err := f.owner.Exec(ctx, `ALTER TABLE control.claim_run_history DROP CONSTRAINT review_reject_release`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.plane.Release(ctx, req); err != nil {
		t.Fatal(err)
	}
	var a, b time.Time
	if err := f.owner.QueryRow(ctx, `SELECT a.released_at,b.released_at FROM control.claim_history a JOIN control.claim_run_history b USING(instance_id,run_id,gaggle,provider,external_id) WHERE a.external_id='atomic'`).Scan(&a, &b); err != nil {
		t.Fatal(err)
	}
	if !a.Equal(b) {
		t.Fatalf("release instants differ %v %v", a, b)
	}
	for _, role := range []string{"goobers_api", "goobers_worker"} {
		var exists bool
		if err := f.owner.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)`, role).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			continue
		}
		var allowed bool
		if err := f.owner.QueryRow(ctx, `SELECT has_any_column_privilege($1,'control.claim_run_history','SELECT,INSERT,UPDATE') OR has_table_privilege($1,'control.claim_run_history','DELETE')`, role).Scan(&allowed); err != nil {
			t.Fatal(err)
		}
		if allowed {
			t.Fatalf("unexpected history grants for %s", role)
		}
	}
}

// ListNamespace is the controller's own namespace read. It sees the namespace's
// holders and history, shows live legacy claims in the ledger's unscoped form,
// and, unlike Store.Snapshot, never reports a failed read as an empty namespace.
func TestPlaneListNamespaceIsTheControllersErrorReturningRead(t *testing.T) {
	f := newPlaneFixture(t)
	leader := f.leader(t, "a")
	plane := leader.plane
	ctx := t.Context()

	if listing, err := plane.ListNamespace(ctx, "g", "github"); err != nil || len(listing.Entries) != 0 || len(listing.History) != 0 {
		t.Fatalf("empty namespace = %+v, %v", listing, err)
	}
	for _, item := range []string{"pr/41", "pr/42"} {
		if got, err := plane.Acquire(ctx, request(item, "run-"+item)); err != nil || !got.Ok {
			t.Fatalf("acquire %s = %+v, %v", item, got, err)
		}
	}
	// Another namespace's claim must not appear.
	other := request("pr/43", "run-x")
	other.Gaggle = "h"
	if got, err := plane.Acquire(ctx, other); err != nil || !got.Ok {
		t.Fatalf("acquire in another namespace = %+v, %v", got, err)
	}
	// A live legacy (unscoped) claim is exclusive against every namespace.
	// Only the scheduler-side store writes them (the plane refuses the namespace).
	store, err := New(f.controller, pgtest.InstanceID, leader.manager, leader.handle.Load())
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := store.Claim("pr/9", "run-legacy", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("legacy claim = %v, %v", ok, err)
	}
	settle := request("pr/41", "run-pr/41")
	settle.Outcome = "completed"
	if got, err := plane.Settle(ctx, settle); err != nil || !got.Ok {
		t.Fatalf("settle = %+v, %v", got, err)
	}

	listing, err := plane.ListNamespace(ctx, "g", "github")
	if err != nil {
		t.Fatal(err)
	}
	var live []string
	for _, entry := range listing.Entries {
		live = append(live, entry.Gaggle+"/"+entry.Provider+"/"+entry.ExternalID)
	}
	if !reflect.DeepEqual(live, []string{"g/github/pr/42", "//pr/9"}) {
		t.Fatalf("live holders = %v, want pr/42 and the legacy claim (unscoped) only", live)
	}
	// History is the plane's own (queryHistory): the settled claim carries its
	// release, and the namespace read agrees with the API's listing of it.
	viaAPI, err := plane.List(ctx, httpapi.ClaimListRequest{Gaggle: "g", Provider: "github", RunID: "run-pr/42", Scope: httpapi.ClaimListScopeNamespace, IncludeHistory: true})
	if err != nil || len(viaAPI.History) != len(listing.History) {
		t.Fatalf("namespace history has %d records, the API's listing has %d (%v)", len(listing.History), len(viaAPI.History), err)
	}
	settled := false
	for _, record := range listing.History {
		if record.ExternalID == "pr/41" && record.ReleasedAt != nil && record.ReleaseReason.Disposition == claimrelease.Completed {
			settled = true
		}
	}
	if !settled {
		t.Fatalf("history = %+v, want the settled pr/41 with its release", listing.History)
	}
	legacyOnly, err := plane.ListNamespace(ctx, "", "")
	if err != nil || len(legacyOnly.Entries) != 1 || legacyOnly.Entries[0].ExternalID != "pr/9" || legacyOnly.Entries[0].Gaggle != "" {
		t.Fatalf("legacy namespace = %+v, %v", legacyOnly, err)
	}
	if _, err := plane.ListNamespace(ctx, "g", ""); err == nil {
		t.Fatal("a half-specified namespace was accepted")
	}
}

func TestPlaneListNamespaceReturnsQueryErrorsInsteadOfAnEmptyNamespace(t *testing.T) {
	f := newPlaneFixture(t)
	plane := f.leader(t, "a").plane
	if got, err := plane.Acquire(t.Context(), request("pr/41", "run-a")); err != nil || !got.Ok {
		t.Fatalf("acquire = %+v, %v", got, err)
	}
	// The same call, with the read failing: Snapshot would answer "no claims".
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	listing, err := plane.ListNamespace(cancelled, "g", "github")
	if err == nil {
		t.Fatalf("a failed read returned %+v with no error: it would read as an unclaimed namespace", listing)
	}
	if len(listing.Entries) != 0 || len(listing.History) != 0 {
		t.Fatalf("a failed read returned partial data: %+v", listing)
	}
}

// A failure of EITHER of the two reads fails ListNamespace: failing only the
// entries query, then only the history query, each yields an error and no
// listing. (A cancelled context fails both at once, so it cannot tell a read
// that swallows one error from one that propagates it.) The failures are column
// privileges revoked from the controller on exactly one table's column, which
// only that table's query selects.
func TestPlaneListNamespaceFailsWhenEitherReadFails(t *testing.T) {
	f := newPlaneFixture(t)
	plane := f.leader(t, "a").plane
	ctx := t.Context()
	for _, item := range []string{"pr/41", "pr/42"} {
		if got, err := plane.Acquire(ctx, request(item, "run-"+item)); err != nil || !got.Ok {
			t.Fatalf("acquire %s = %+v, %v", item, got, err)
		}
	}
	settle := request("pr/41", "run-pr/41")
	settle.Outcome = "completed"
	if got, err := plane.Settle(ctx, settle); err != nil || !got.Ok {
		t.Fatalf("settle = %+v, %v", got, err)
	}
	if listing, err := plane.ListNamespace(ctx, "g", "github"); err != nil || len(listing.Entries) == 0 || len(listing.History) == 0 {
		t.Fatalf("baseline listing = %+v, %v; both reads need rows for this test to mean anything", listing, err)
	}

	role := pgx.Identifier{pgroles.Controller}.Sanitize()
	for name, table := range map[string]string{"entries": "control.claim", "history": "control.claim_history"} {
		if _, err := f.owner.Exec(ctx, "REVOKE SELECT (verification) ON "+table+" FROM "+role); err != nil {
			t.Fatal(err)
		}
		listing, err := plane.ListNamespace(ctx, "g", "github")
		if err == nil {
			t.Fatalf("with the %s read failing ListNamespace returned %+v and no error", name, listing)
		}
		if len(listing.Entries) != 0 || len(listing.History) != 0 {
			t.Fatalf("with the %s read failing ListNamespace returned partial data: %+v", name, listing)
		}
		if _, err := f.owner.Exec(ctx, "GRANT SELECT (verification) ON "+table+" TO "+role); err != nil {
			t.Fatal(err)
		}
		if _, err := plane.ListNamespace(ctx, "g", "github"); err != nil {
			t.Fatalf("restoring the %s read did not restore ListNamespace: %v", name, err)
		}
	}
}
