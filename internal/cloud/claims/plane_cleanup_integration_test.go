//go:build cloudintegration

package claims

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/localscheduler"
)

// fakeMarkers records marker releases and answers with err.
type fakeMarkers struct {
	mu    sync.Mutex
	calls []localscheduler.ClaimEntry
	err   error
}

func (m *fakeMarkers) release(_ context.Context, claim localscheduler.ClaimEntry, _ claimrelease.Reason) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, claim)
	return m.err
}

func (m *fakeMarkers) fail(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

func (m *fakeMarkers) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// cleanupPlane is r's plane with a marker releaser.
func cleanupPlane(t *testing.T, f planeFixture, r *replica, markers *fakeMarkers) *Plane {
	t.Helper()
	plane, err := NewPlane(PlaneConfig{
		Pool: f.controller, InstanceID: pgtest.InstanceID, Lease: r.manager,
		Owner: r.handle.Load, DefaultLease: 30 * time.Minute, ReleaseMarker: markers.release,
	})
	if err != nil {
		t.Fatal(err)
	}
	return plane
}

func claimedAt(t *testing.T, p *Plane, run, item string) time.Time {
	t.Helper()
	listed, err := p.List(t.Context(), httpapi.ClaimListRequest{RunID: run, Scope: httpapi.ClaimListScopeRun})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range listed.Entries {
		if entry.ItemID == item {
			return entry.ClaimedAt
		}
	}
	t.Fatalf("%s holds no %s: %+v", run, item, listed.Entries)
	return time.Time{}
}

func cleanupRequest(item, run string, at time.Time) httpapi.ClaimRequest {
	req := request(item, run)
	req.ProviderCleanup, req.ClaimedAt = true, at
	req.Reason = claimrelease.Abandoned.Because("backlog_query_failed", "")
	return req
}

func TestPlaneProviderCleanupReleasesTheExactIncarnation(t *testing.T) {
	f := newPlaneFixture(t)
	r := f.leader(t, "a")
	markers := &fakeMarkers{}
	p := cleanupPlane(t, f, r, markers)
	ctx := t.Context()
	if _, err := p.Acquire(ctx, request("42", "run-a")); err != nil {
		t.Fatal(err)
	}
	at := claimedAt(t, p, "run-a", "42")
	if got, err := p.Release(ctx, cleanupRequest("42", "run-a", at.Add(-time.Second))); err != nil || got.Ok || markers.count() != 0 {
		t.Fatalf("stale incarnation cleanup = %+v, %v (%d marker calls); want nothing reserved", got, err, markers.count())
	}
	got, err := p.Release(ctx, cleanupRequest("42", "run-a", at))
	if err != nil || !got.Ok || markers.count() != 1 || markers.calls[0].RunID != "run-a" || markers.calls[0].ItemID != "42" {
		t.Fatalf("cleanup = %+v, %v, marker calls %+v", got, err, markers.calls)
	}
	listed, err := p.List(ctx, httpapi.ClaimListRequest{RunID: "run-a", Scope: httpapi.ClaimListScopeRun, IncludeHistory: true})
	if err != nil || len(listed.Entries) != 0 || len(listed.History) != 1 || listed.History[0].ReleasePending ||
		listed.History[0].ReleaseReason.Code != "backlog_query_failed" {
		t.Fatalf("after cleanup = %+v, %v", listed, err)
	}
	if again, err := p.Release(ctx, cleanupRequest("42", "run-a", at)); err != nil || again.Ok {
		t.Fatalf("retried cleanup after finish = %+v, %v; want Ok=false (nothing held)", again, err)
	}
}

// TestPlaneCleanupCustodyBlocksEveryOtherPath: a failed cleanup keeps custody,
// and while it is held no run may take, renew or plainly release the claim
// and no sweep may reap it; a retry finishes it (ClaimLedger transitionPending).
func TestPlaneCleanupCustodyBlocksEveryOtherPath(t *testing.T) {
	f := newPlaneFixture(t)
	r := f.leader(t, "a")
	markers := &fakeMarkers{err: errors.New("provider unavailable")}
	p := cleanupPlane(t, f, r, markers)
	ctx := t.Context()
	if _, err := p.Acquire(ctx, request("42", "run-a")); err != nil {
		t.Fatal(err)
	}
	at := claimedAt(t, p, "run-a", "42")
	if _, err := p.Release(ctx, cleanupRequest("42", "run-a", at)); planeCode(err) != "503 provider_cleanup_failed" {
		t.Fatalf("failed cleanup = %v", err)
	}
	listed, err := p.List(ctx, httpapi.ClaimListRequest{RunID: "run-a", Scope: httpapi.ClaimListScopeRun, IncludeHistory: true})
	if err != nil || len(listed.Entries) != 1 || !listed.Entries[0].ReleasePending || len(listed.History) != 1 || !listed.History[0].ReleasePending {
		t.Fatalf("custody not visible: %+v, %v", listed, err)
	}
	for name, req := range map[string]httpapi.ClaimRequest{"other run": request("42", "run-b"), "own run": request("42", "run-a")} {
		if got, err := p.Acquire(ctx, req); err != nil || got.Ok || got.Holder != "run-a" {
			t.Fatalf("%s acquire under custody = %+v, %v", name, got, err)
		}
	}
	if got, err := p.Renew(ctx, request("42", "run-a")); err != nil || got.Ok {
		t.Fatalf("renew under custody = %+v, %v", got, err)
	}
	if _, err := p.Release(ctx, request("42", "run-a")); planeCode(err) != "409 claim_release_pending" {
		t.Fatalf("plain release under custody = %v", err)
	}
	if got, err := p.Release(ctx, httpapi.ClaimRequest{RunID: "run-a"}); err != nil || len(got.Released) != 0 {
		t.Fatalf("release-all under custody = %+v, %v", got, err)
	}
	if got, err := p.ReleaseRun(ctx, r.handle.Load(), "run-a", claimrelease.Failed.Reason()); err != nil || len(got) != 0 {
		t.Fatalf("ReleaseRun under custody = %+v, %v", got, err)
	}
	if renewed, err := p.RenewRuns(ctx, r.handle.Load(), map[string]bool{"run-a": true}); err != nil || len(renewed) != 0 {
		t.Fatalf("RenewRuns under custody = %+v, %v", renewed, err)
	}
	if _, err := f.owner.Exec(ctx, `UPDATE control.claim SET expires_at=clock_timestamp()-interval '1 second' WHERE instance_id=$1`, pgtest.InstanceID); err != nil {
		t.Fatal(err)
	}
	gate := localscheduler.NewRecoveryGate()
	gate.MarkRenewalRebuilt()
	if got, err := p.RecoverExpired(ctx, r.handle.Load(), gate, func(context.Context, string) (bool, error) { return true, nil }); err != nil || len(got) != 0 {
		t.Fatalf("RecoverExpired under custody = %+v, %v", got, err)
	}
	if got, err := p.Recover(ctx, httpapi.ClaimRecoverRequest{RunID: "run-a"}); err != nil || len(got.Released) != 0 {
		t.Fatalf("Recover under custody = %+v, %v", got, err)
	}
	store, err := New(f.controller, pgtest.InstanceID, r.manager, r.handle.Load())
	if err != nil {
		t.Fatal(err)
	}
	key := localscheduler.ClaimKey{Gaggle: "g", Provider: "github", ExternalID: "42"}
	if ok, _, err := store.ClaimScoped(key, "run-b", "implementation", time.Hour); err != nil || ok {
		t.Fatalf("scheduler claim under custody = %v, %v", ok, err)
	}
	if err := store.ReleaseScoped(key, "run-a", claimrelease.Failed.Reason()); !errors.Is(err, ErrTransition) {
		t.Fatalf("scheduler release under custody = %v, want refused", err)
	}

	markers.fail(nil)
	if got, err := p.Release(ctx, cleanupRequest("42", "run-a", at)); err != nil || !got.Ok {
		t.Fatalf("retried cleanup = %+v, %v", got, err)
	}
	if got, err := p.Acquire(ctx, request("42", "run-b")); err != nil || !got.Ok {
		t.Fatalf("acquire after cleanup = %+v, %v", got, err)
	}
}

func TestPlaneCleanupRefusalIsPermanentAndKeepsCustody(t *testing.T) {
	f := newPlaneFixture(t)
	r := f.leader(t, "a")
	markers := &fakeMarkers{err: fmt.Errorf("successor holds the item: %w", ErrProviderCleanupRefused)}
	p := cleanupPlane(t, f, r, markers)
	ctx := t.Context()
	if _, err := p.Acquire(ctx, request("42", "run-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Release(ctx, cleanupRequest("42", "run-a", claimedAt(t, p, "run-a", "42"))); planeCode(err) != "409 provider_cleanup_refused" {
		t.Fatalf("refused cleanup = %v", err)
	}
	if got, err := p.Acquire(ctx, request("42", "run-b")); err != nil || got.Ok {
		t.Fatalf("acquire after a refused cleanup = %+v, %v; want custody kept", got, err)
	}
	pub := localscheduler.PublicationLockItemIDPrefix + "o/r/1"
	if _, err := p.Release(ctx, cleanupRequest(pub, "run-a", time.Now())); planeCode(err) != "409 publication_custody" {
		t.Fatalf("publication cleanup = %v", err)
	}
}

// TestPlaneCleanupResumesAcrossCrashAndTakeover: custody reserved by one
// epoch (its marker call never finished) is resumed by a retry on the next
// leader, which adopts the row and repeats the idempotent marker release.
func TestPlaneCleanupResumesAcrossCrashAndTakeover(t *testing.T) {
	f := newPlaneFixture(t)
	first := f.leader(t, "a")
	firstPlane := cleanupPlane(t, f, first, &fakeMarkers{})
	ctx := t.Context()
	if _, err := firstPlane.Acquire(ctx, request("42", "run-a")); err != nil {
		t.Fatal(err)
	}
	at := claimedAt(t, firstPlane, "run-a", "42")
	key := localscheduler.ClaimKey{Gaggle: "g", Provider: "github", ExternalID: "42"}
	if _, reserved, err := firstPlane.beginCleanup(ctx, key, cleanupRequest("42", "run-a", at)); err != nil || !reserved {
		t.Fatalf("begin = %v, %v", reserved, err)
	}
	second := f.takeover(t, "b")
	markers := &fakeMarkers{}
	secondPlane := cleanupPlane(t, f, second, markers)
	if _, err := firstPlane.Release(ctx, cleanupRequest("42", "run-a", at)); planeCode(err) != "503 recovering" {
		t.Fatalf("stale leader cleanup = %v, want retryable 503 recovering", err)
	}
	got, err := secondPlane.Release(ctx, cleanupRequest("42", "run-a", at))
	if err != nil || !got.Ok || markers.count() != 1 {
		t.Fatalf("resumed cleanup on the new leader = %+v, %v (%d marker calls)", got, err, markers.count())
	}
}
