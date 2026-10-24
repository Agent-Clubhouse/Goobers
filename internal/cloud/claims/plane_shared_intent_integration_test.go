//go:build cloudintegration

package claims

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/sharedclaim"
)

func TestSharedRenewRequiresLiveHeldIncarnation(t *testing.T) {
	f := newPlaneFixture(t)
	r := f.leader(t, "a")
	remote := &sharedRemote{}
	p := sharedPlane(t, f, r, remote)
	req := request("42", "run-a")
	for _, workflow := range []string{"implementation", ""} {
		req.Workflow = workflow
		if got, err := p.Renew(t.Context(), req); err != nil || got.Ok {
			t.Fatalf("renew missing custody=%+v, %v", got, err)
		}
	}
	req.Workflow = "implementation"
	if got, err := p.Acquire(t.Context(), req); err != nil || !got.Ok {
		t.Fatalf("acquire=%+v, %v", got, err)
	}
	old := sharedEntry(t, p, req.RunID)
	if _, err := f.owner.Exec(t.Context(), `UPDATE control.claim SET expires_at=now()-interval '1 second',
		shared_deadline=now()-interval '1 second' WHERE instance_id=$1`, p.instanceID); err != nil {
		t.Fatal(err)
	}
	for _, workflow := range []string{"implementation", ""} {
		req.Workflow = workflow
		if got, err := p.Renew(t.Context(), req); err != nil || got.Ok {
			t.Fatalf("renew expired custody=%+v, %v", got, err)
		}
	}
	if remote.revision != 1 {
		t.Fatal("refused renewal touched the remote store")
	}
	if got, err := p.Release(t.Context(), req); err != nil || !got.Ok {
		t.Fatalf("release=%+v, %v", got, err)
	}
	req.Workflow = "implementation"
	if got, err := p.Acquire(t.Context(), req); err != nil || !got.Ok {
		t.Fatalf("new incarnation=%+v, %v", got, err)
	}
	binding, err := p.shared.Admission(t.Context(), claimsclient.KeyForEntry(old), req.RunID, req.Workflow)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := p.claimShared(t.Context(), r.handle.Load(), req, time.Minute, *binding, &old); err != nil || got.Ok {
		t.Fatalf("stale incarnation renewal=%+v, %v", got, err)
	}
}

func TestSharedProviderCallsHoldNoDatabaseConnection(t *testing.T) {
	f := newPlaneFixture(t)
	r := f.leader(t, "a")
	check := func() {
		if held := f.controller.Stat().AcquiredConns(); held != 0 {
			t.Errorf("provider I/O retained %d database connections", held)
		}
	}
	remote := &sharedRemote{beforeRead: check, beforeCAS: check}
	p := sharedPlane(t, f, r, remote)
	req := request("42", "run-a")
	if got, err := p.Acquire(t.Context(), req); err != nil || !got.Ok {
		t.Fatalf("acquire=%+v, %v", got, err)
	}
	if got, err := p.Release(t.Context(), req); err != nil || !got.Ok {
		t.Fatalf("release=%+v, %v", got, err)
	}
}

func TestSharedReleaseTombstoneRejectsDelayedAdmissionCAS(t *testing.T) {
	f := newPlaneFixture(t)
	r := f.leader(t, "a")
	entered, resume := make(chan struct{}), make(chan struct{})
	var closeResume sync.Once
	t.Cleanup(func() { closeResume.Do(func() { close(resume) }) })
	var calls atomic.Int32
	remote := &sharedRemote{beforeCAS: func() {
		if calls.Add(1) == 1 {
			// This is after the durable-intent recheck, inside the residual
			// network window. Cleanup must fence it using remote revision CAS.
			close(entered)
			<-resume
		}
	}}
	p := sharedPlane(t, f, r, remote)
	req := request("42", "run-a")
	done := make(chan error, 1)
	go func() { _, err := p.Acquire(t.Context(), req); done <- err }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("admission CAS never started")
	}
	if got, err := p.Release(t.Context(), req); err != nil || !got.Ok {
		t.Fatalf("release pending admission=%+v, %v", got, err)
	}
	observed, err := remote.Read(t.Context(), req.ItemID)
	if err != nil || observed.Revision == "" || observed.Record.Owner != (sharedclaim.Owner{}) {
		t.Fatalf("missing release tombstone=%+v, %v", observed, err)
	}
	closeResume.Do(func() { close(resume) })
	if err := <-done; !errors.Is(err, sharedclaim.ErrConflict) {
		t.Fatalf("stale admission CAS=%v", err)
	}
	listing, err := p.List(t.Context(), httpapi.ClaimListRequest{RunID: req.RunID, Scope: httpapi.ClaimListScopeRun, IncludeHistory: true})
	if err != nil || len(listing.Entries) != 0 || len(listing.History) != 1 {
		t.Fatalf("stale admission restored execution=%+v, %v", listing, err)
	}
	observed, err = remote.Read(t.Context(), req.ItemID)
	if err != nil || observed.Record.Owner != (sharedclaim.Owner{}) {
		t.Fatalf("stale admission overwrote tombstone=%+v, %v", observed, err)
	}
}

func TestSharedForceReleaseRevokesWithoutProviderResolver(t *testing.T) {
	f := newPlaneFixture(t)
	r := f.leader(t, "a")
	remote := &sharedRemote{}
	p := sharedPlane(t, f, r, remote)
	req := request("42", "run-a")
	if got, err := p.Acquire(t.Context(), req); err != nil || !got.Ok {
		t.Fatalf("acquire=%+v, %v", got, err)
	}
	p.shared = nil
	req.Reason = claimrelease.ForceReleased.Because("operator_requested", "")
	req.Reason.Actor = "operator"
	if _, err := p.Release(t.Context(), req); planeCode(err) != "503 provider_cleanup_failed" {
		t.Fatalf("missing resolver cleanup=%v", err)
	}
	entry := sharedEntry(t, p, req.RunID)
	if !entry.SharedRevoked || entry.ExpiresAt.After(time.Now()) {
		t.Fatalf("missing resolver retained execution=%+v", entry)
	}
	listing, err := p.List(t.Context(), httpapi.ClaimListRequest{RunID: req.RunID, Scope: httpapi.ClaimListScopeRun, IncludeHistory: true})
	if err != nil || len(listing.History) != 1 || !listing.History[0].SharedRevoked {
		t.Fatalf("revocation was not durable=%+v, %v", listing, err)
	}
	p.shared = sharedResolver{remote}
	if got, err := p.Acquire(t.Context(), request("42", "run-a")); err == nil || got.Ok {
		t.Fatalf("revoked run readmitted=%+v, %v", got, err)
	}
	if got, err := p.Release(t.Context(), req); err != nil || !got.Ok {
		t.Fatalf("cleanup retry=%+v, %v", got, err)
	}
}
