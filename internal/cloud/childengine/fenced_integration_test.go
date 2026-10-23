//go:build cloudintegration

package childengine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"

	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/objstore"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// unusedCredential lets the object-store client be built without contacting
// any service; these tests never reach a generation archive.
type unusedCredential struct{}

func (unusedCredential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{}, errors.New("unused")
}

func fencedFor(t *testing.T, f *family, owner func() *cloudlease.Handle) Fenced {
	t.Helper()
	azure, err := objstore.NewAzure("http://127.0.0.1:1", "unused", unusedCredential{})
	if err != nil {
		t.Fatal(err)
	}
	objects, err := objstore.NewMetadataWithOptions(f.pool, azure, objstore.MetadataOptions{})
	if err != nil {
		t.Fatal(err)
	}
	fenced := Fenced{Pool: f.pool, InstanceID: testInstance, Lease: f.manager, Owner: owner, Blobs: f.blobs, Objects: objects,
		Applied: appliedConfig{}, Wait: time.Hour, Actor: "control-plane", Now: time.Now}
	if err := fenced.Validate(); err != nil {
		t.Fatal(err)
	}
	return fenced
}

func TestFencedAdmissionsBindEveryCallToTheLiveLease(t *testing.T) {
	f := newFamily(t)
	in := acceptChild(t, f, "parent-fenced", "call-1")
	binding := engine.ChildRunBinding{Request: in.Request}
	var owner *cloudlease.Handle
	fenced := fencedFor(t, f, func() *cloudlease.Handle { return owner })

	// A standby (or a replica mid-failover) holds no lease: retry elsewhere.
	if err := fenced.Cancel(t.Context(), binding); !errors.Is(err, engine.ErrChildCustodyUnavailable) {
		t.Fatalf("cancel without a lease = %v, want unavailable", err)
	}
	if err := f.store.CheckChildParentOpen(t.Context(), identityOf(in.Request).ChildParent); err != nil {
		t.Fatalf("a call without a lease changed custody: %v", err)
	}

	// The live owner fences the family.
	owner = f.handle
	if err := fenced.Cancel(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	if err := f.store.CheckChildParentOpen(t.Context(), identityOf(in.Request).ChildParent); !errors.Is(err, triggerqueue.ErrParentCancelled) {
		t.Fatalf("live-lease cancel did not fence the family: %v", err)
	}

	// The live owner reaches the real freezer. This fixture's retained start
	// payload is not an envelope, so the child can never freeze: a permanent
	// refusal, and nothing is bound.
	other := acceptChild(t, f, "parent-fenced-2", "call-1")
	if _, err := fenced.Admit(t.Context(), other); !errors.Is(err, engine.ErrChildCustodyRefused) {
		t.Fatalf("admit with a corrupt retained envelope = %v, want refused", err)
	}
	if _, err := f.store.ChildEngineStart(t.Context(), identityOf(other.Request)); err == nil {
		t.Fatal("a refused freeze bound a start intent")
	}

	// A lost epoch can never write: the stale handle is refused as unavailable.
	if err := f.manager.Release(t.Context(), f.handle); err != nil {
		t.Fatal(err)
	}
	if err := fenced.Cancel(t.Context(), engine.ChildRunBinding{Request: other.Request}); !errors.Is(err, engine.ErrChildCustodyUnavailable) {
		t.Fatalf("cancel under a released lease = %v, want unavailable", err)
	}
}
