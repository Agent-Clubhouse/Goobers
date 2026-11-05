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
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/sharedclaim"
)

type sharedRemote struct {
	mu         sync.Mutex
	record     sharedclaim.Record
	revision   int
	lost       bool
	fail       error
	beforeRead func()
	beforeCAS  func()
}

func (s *sharedRemote) Read(context.Context, string) (sharedclaim.Observation, error) {
	if s.beforeRead != nil {
		s.beforeRead()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	revision := ""
	if s.revision > 0 {
		revision = fmt.Sprint(s.revision)
	}
	return sharedclaim.Observation{Record: s.record, Revision: revision, Now: time.Now()}, s.fail
}

func (s *sharedRemote) CompareAndSwap(_ context.Context, _, revision string, record sharedclaim.Record) error {
	if s.beforeCAS != nil {
		s.beforeCAS()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	want := ""
	if s.revision > 0 {
		want = fmt.Sprint(s.revision)
	}
	if revision != want {
		return sharedclaim.ErrConflict
	}
	s.record, s.revision = record, s.revision+1
	if s.lost {
		s.lost = false
		return errors.New("lost acknowledgment")
	}
	return nil
}

type sharedResolver struct{ remote *sharedRemote }

func (r sharedResolver) Visibility(context.Context, string) (string, error) { return "shared", nil }
func (r sharedResolver) Admission(_ context.Context, key claimsclient.Key, run, _ string) (*claimsclient.SharedClaimBinding, error) {
	return &claimsclient.SharedClaimBinding{Store: r.remote, RemoteKey: key.ExternalID, Owner: sharedclaim.Owner{Instance: pgtest.InstanceID, Run: run, Token: "owner-" + run}}, nil
}
func (r sharedResolver) Release(ctx context.Context, entry claimsclient.Entry) (claimsclient.SharedClaimBinding, error) {
	binding, err := r.Admission(ctx, claimsclient.KeyForEntry(entry), entry.RunID, entry.Workflow)
	return *binding, err
}
func sharedPlane(t *testing.T, f planeFixture, r *replica, remote *sharedRemote) *Plane {
	t.Helper()
	p, err := NewPlane(PlaneConfig{Pool: f.controller, InstanceID: pgtest.InstanceID, Lease: r.manager, Owner: r.handle.Load,
		DefaultLease: time.Minute, Shared: sharedResolver{remote}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func sharedEntry(t *testing.T, p *Plane, run string) localscheduler.ClaimEntry {
	t.Helper()
	entries, err := p.selectEntries(t.Context(), p.pool, `run_id=$2`, run)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries=%+v, %v", entries, err)
	}
	return entries[0]
}

func TestSharedAdmissionLostACKRetriesThePersistedOwner(t *testing.T) {
	f := newPlaneFixture(t)
	r := f.leader(t, "a")
	remote := &sharedRemote{lost: true}
	p := sharedPlane(t, f, r, remote)
	ctx := t.Context()
	req := request("42", "run-a")
	if got, err := p.Acquire(ctx, req); err == nil || got.Ok {
		t.Fatalf("uncertain admission=%+v, %v", got, err)
	}
	pending := sharedEntry(t, p, req.RunID)
	if !pending.SharedAdmissionPending || pending.ExpiresAt.After(time.Now()) || !pending.SharedDeadline.Equal(pending.ExpiresAt) {
		t.Fatalf("pending=%+v", pending)
	}
	// A replacement controller resumes exactly the same remote incarnation.
	next := f.takeover(t, "b")
	p = sharedPlane(t, f, next, remote)
	got, err := p.Acquire(ctx, req)
	if err != nil || !got.Ok || got.ExpiresAt == nil {
		t.Fatalf("retry=%+v, %v", got, err)
	}
	admitted := sharedEntry(t, p, req.RunID)
	if admitted.SharedOwner != pending.SharedOwner || !admitted.ClaimedAt.Equal(pending.ClaimedAt) || admitted.SharedAdmissionPending ||
		!admitted.SharedDeadline.Equal(admitted.ExpiresAt) || !admitted.ExpiresAt.Equal(*got.ExpiresAt) {
		t.Fatalf("admitted=%+v", admitted)
	}
	snapshot, err := p.List(ctx, httpapi.ClaimListRequest{RunID: req.RunID, Scope: httpapi.ClaimListScopeRun, Execution: true, IncludeHistory: true})
	if err != nil || snapshot.ClaimVisibility != "shared" || len(snapshot.Entries) != 1 || len(snapshot.History) != 1 {
		t.Fatalf("snapshot=%+v, %v", snapshot, err)
	}
	if got, err := p.Acquire(ctx, request("42", "run-b")); err != nil || got.Ok {
		t.Fatalf("competitor=%+v, %v", got, err)
	}
}

func TestSharedReleaseKeepsSuccessorAndRetriesUncertainCleanup(t *testing.T) {
	f := newPlaneFixture(t)
	r := f.leader(t, "a")
	remote := &sharedRemote{}
	p := sharedPlane(t, f, r, remote)
	ctx := t.Context()
	req := request("42", "run-a")
	if got, err := p.Acquire(ctx, req); err != nil || !got.Ok {
		t.Fatalf("acquire=%+v, %v", got, err)
	}
	remote.mu.Lock()
	remote.fail = errors.New("provider unavailable")
	remote.mu.Unlock()
	if _, err := p.Release(ctx, req); err == nil {
		t.Fatal("provider failure was swallowed")
	}
	if !sharedEntry(t, p, req.RunID).ReleasePending {
		t.Fatal("cleanup custody lost")
	}
	if got, err := p.Renew(ctx, req); err != nil || got.Ok {
		t.Fatalf("renew pending=%+v, %v", got, err)
	}
	successor := sharedclaim.Owner{Instance: "other", Run: "successor", Token: "new"}
	remote.mu.Lock()
	remote.fail = nil
	remote.record = sharedclaim.Record{Version: 1, Owner: successor, ExpiresAt: time.Now().Add(time.Minute)}
	remote.revision++
	remote.mu.Unlock()
	if got, err := p.Release(ctx, req); err != nil || !got.Ok {
		t.Fatalf("release gone owner=%+v, %v", got, err)
	}
	observed, err := remote.Read(ctx, "42")
	if err != nil || observed.Record.Owner != successor {
		t.Fatalf("successor changed=%+v, %v", observed, err)
	}
	listing, err := p.List(ctx, httpapi.ClaimListRequest{RunID: req.RunID, Scope: httpapi.ClaimListScopeRun, IncludeHistory: true})
	if err != nil || len(listing.Entries) != 0 || len(listing.History) != 1 || listing.History[0].ReleasePending {
		t.Fatalf("cleanup=%+v, %v", listing, err)
	}
}

func TestSharedRevocationWinsAnInflightAdmission(t *testing.T) {
	f := newPlaneFixture(t)
	r := f.leader(t, "a")
	entered := make(chan struct{})
	resume := make(chan struct{})
	var once sync.Once
	remote := &sharedRemote{beforeCAS: func() { once.Do(func() { close(entered); <-resume }) }}
	p := sharedPlane(t, f, r, remote)
	ctx := t.Context()
	req := request("42", "run-a")
	done := make(chan error, 1)
	go func() { _, err := p.Acquire(ctx, req); done <- err }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("CAS never started")
	}
	entry := sharedEntry(t, p, req.RunID)
	err := p.revokeShared(ctx, r.handle.Load(), entry, "operator")
	close(resume)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("revoked admission finalized")
	}
	if !sharedEntry(t, p, req.RunID).SharedRevoked {
		t.Fatal("revocation lost")
	}
	req.Reason = claimrelease.ForceReleased.Because("operator_requested", "")
	req.Reason.Actor = "operator"
	if _, err := p.Release(ctx, req); err != nil {
		t.Fatal(err)
	}
	if got, err := p.Acquire(ctx, request("42", "run-a")); err == nil || got.Ok {
		t.Fatalf("revoked run readmitted=%+v, %v", got, err)
	}
	if got, err := p.Acquire(ctx, request("42", "run-b")); err != nil || !got.Ok {
		t.Fatalf("new run refused=%+v, %v", got, err)
	}
}

func TestSharedTakeoverFencesDelayedFinalization(t *testing.T) {
	f := newPlaneFixture(t)
	r := f.leader(t, "a")
	entered := make(chan struct{})
	resume := make(chan struct{})
	var once sync.Once
	remote := &sharedRemote{beforeCAS: func() { once.Do(func() { close(entered); <-resume }) }}
	p := sharedPlane(t, f, r, remote)
	ctx := t.Context()
	req := request("42", "run-a")
	done := make(chan error, 1)
	go func() { _, err := p.Acquire(ctx, req); done <- err }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("CAS never started")
	}
	next := f.takeover(t, "b")
	close(resume)
	if err := <-done; planeCode(err) != "503 recovering" {
		t.Fatalf("old finalize=%v", err)
	}
	if !sharedEntry(t, p, req.RunID).SharedAdmissionPending {
		t.Fatal("old controller granted execution")
	}
	p = sharedPlane(t, f, next, remote)
	if got, err := p.Acquire(ctx, req); err != nil || !got.Ok {
		t.Fatalf("takeover=%+v, %v", got, err)
	}
	if got, err := p.ReleaseRun(ctx, next.handle.Load(), req.RunID, claimrelease.Completed.Because("test", "")); err != nil || len(got) != 1 {
		t.Fatalf("terminal release=%+v, %v", got, err)
	}
}

func TestSharedRenewalAndExpiredRecoveryUseRemoteCustody(t *testing.T) {
	f := newPlaneFixture(t)
	r := f.leader(t, "a")
	remote := &sharedRemote{}
	p := sharedPlane(t, f, r, remote)
	ctx := t.Context()
	req := request("42", "run-a")
	if got, err := p.Acquire(ctx, req); err != nil || !got.Ok {
		t.Fatalf("acquire=%+v, %v", got, err)
	}
	req.Workflow = ""
	if got, err := p.Renew(ctx, req); err != nil || !got.Ok {
		t.Fatalf("renew without workflow=%+v, %v", got, err)
	}
	if got, err := p.RenewRuns(ctx, r.handle.Load(), map[string]bool{req.RunID: true}); err != nil || len(got) != 1 {
		t.Fatalf("epoch renew=%+v, %v", got, err)
	}
	if _, err := f.owner.Exec(ctx, `UPDATE control.claim SET expires_at=now()-interval '1 second',
		shared_deadline=now()-interval '1 second' WHERE instance_id=$1`, p.instanceID); err != nil {
		t.Fatal(err)
	}
	if got, err := p.Recover(ctx, httpapi.ClaimRecoverRequest{}); err != nil || len(got.Released) != 1 {
		t.Fatalf("expiry=%+v, %v", got, err)
	}
	observed, err := remote.Read(ctx, "42")
	if err != nil || observed.Record.Owner != (sharedclaim.Owner{}) || observed.Revision == "" {
		t.Fatalf("expired remote custody=%+v, %v", observed, err)
	}
}

func TestSharedForceReleaseRevokesBeforeProviderFailure(t *testing.T) {
	f := newPlaneFixture(t)
	r := f.leader(t, "a")
	remote := &sharedRemote{}
	p := sharedPlane(t, f, r, remote)
	ctx := t.Context()
	req := request("42", "run-a")
	if got, err := p.Acquire(ctx, req); err != nil || !got.Ok {
		t.Fatalf("acquire=%+v, %v", got, err)
	}
	remote.mu.Lock()
	remote.fail = errors.New("credentials unavailable")
	remote.mu.Unlock()
	req.Reason = claimrelease.ForceReleased.Because("operator_requested", "")
	req.Reason.Actor = "operator"
	if _, err := p.Release(ctx, req); err == nil {
		t.Fatal("provider failure hidden")
	}
	entry := sharedEntry(t, p, req.RunID)
	if !entry.SharedRevoked || entry.SharedDeadline.After(time.Now()) || !entry.SharedDeadline.Equal(entry.ExpiresAt) {
		t.Fatalf("revocation=%+v", entry)
	}
	if got, err := p.Acquire(ctx, request("42", "run-a")); err == nil || got.Ok {
		t.Fatalf("revoked admission=%+v, %v", got, err)
	}
	remote.mu.Lock()
	remote.fail = nil
	remote.mu.Unlock()
	if got, err := p.Release(ctx, req); err != nil || !got.Ok {
		t.Fatalf("force release retry=%+v, %v", got, err)
	}
	if got, err := p.Acquire(ctx, request("42", "run-a")); err == nil || got.Ok {
		t.Fatalf("revoked history readmission=%+v, %v", got, err)
	}
}
