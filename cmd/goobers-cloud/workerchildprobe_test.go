package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/childresult"
	"github.com/goobers/goobers/internal/engine"
)

type grantingAuthority struct{ acquired int }

func (g *grantingAuthority) AcquireChildAuthority(context.Context, engine.ChildAuthorityRequest) (engine.ChildAuthorityGrant, error) {
	g.acquired++
	return engine.ChildAuthorityGrant{GrantID: "g"}, nil
}
func (g *grantingAuthority) ReleaseChildAuthority(context.Context, engine.ChildAuthorityRelease) error {
	return nil
}

// A worker lends stages child-workflow authority only when control's canary
// for this instance is readable through the stage blob plane; a plane that
// does not serve control's store withholds it and logs why (#8085 d, #8188).
func TestWorkerChildAuthorityRequiresControlsCanaryOnTheBlobPlane(t *testing.T) {
	const instanceID = "instance-probe"
	for _, served := range []bool{false, true} {
		store, err := blobstore.NewDir(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if served {
			digest, data := childresult.BlobPlaneCanary(instanceID)
			if err := store.Put(t.Context(), digest, data); err != nil {
				t.Fatal(err)
			}
		}
		var logged []string
		seams := &workerSeams{store: store, logf: func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }}
		inner := &grantingAuthority{}
		authority := probedChildAuthority(inner, seams, instanceID)
		_, err = authority.AcquireChildAuthority(t.Context(), engine.ChildAuthorityRequest{})
		switch {
		case served && (err != nil || inner.acquired != 1 || len(logged) != 0):
			t.Fatalf("plane serving control's canary: acquire = %v (control reached %d, logged %q)", err, inner.acquired, logged)
		case !served && (!errors.Is(err, childresult.ErrBlobPlaneUnverified) || inner.acquired != 0 || len(logged) == 0):
			t.Fatalf("plane without control's canary: acquire = %v (control reached %d, logged %q), want withheld and logged", err, inner.acquired, logged)
		}
	}
	if probedChildAuthority(nil, &workerSeams{}, instanceID) != nil {
		t.Fatal("a worker without child authority gained one")
	}
}

// Control puts exactly the canary a worker for the same instance probes for,
// and refuses to compose the protocol without an object store.
func TestControlPutsTheCanaryWorkersProbeFor(t *testing.T) {
	store, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := putBlobPlaneCanary(t.Context(), store, "instance-probe"); err != nil {
		t.Fatal(err)
	}
	if err := putBlobPlaneCanary(t.Context(), store, "instance-probe"); err != nil {
		t.Fatalf("a repeated put at the next boot = %v", err)
	}
	inner := &grantingAuthority{}
	if _, err := probedChildAuthority(inner, &workerSeams{store: store}, "instance-probe").AcquireChildAuthority(t.Context(), engine.ChildAuthorityRequest{}); err != nil || inner.acquired != 1 {
		t.Fatalf("a worker probing control's own canary = %v (reached %d)", err, inner.acquired)
	}
	if err := putBlobPlaneCanary(t.Context(), nil, "instance-probe"); err == nil {
		t.Fatal("composed the protocol without an object store")
	}
}
