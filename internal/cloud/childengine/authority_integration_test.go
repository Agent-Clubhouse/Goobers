//go:build cloudintegration

package childengine

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/triggerqueue"
)

const podEndpoint = "https://control.example"

// authorityFamily is an engine parent whose pins live in PostgreSQL, served by
// Authorities over the controller role's lease-fenced custody.
type authorityFamily struct {
	*family
	parent     *parentFixture
	controller *acceptance.Store
	grants     *podauth.SignedKey
}

func newAuthorityFamily(t *testing.T) *authorityFamily {
	t.Helper()
	f := &authorityFamily{family: newFamily(t), parent: newParentFixture(t)}
	pins := f.parent.pins
	repo, err := json.Marshal(pins.RepoRef)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), `
		INSERT INTO control.run_pin (instance_id, run_id, acceptance_id, gaggle, workflow, workflow_version, workflow_digest,
			goober_digest, config_generation, repo_ref, branch_namespace, input_digest, owner_epoch)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		testInstance, pins.RunID, pins.AcceptanceID, pins.Gaggle, pins.Workflow, pins.WorkflowVersion, pins.WorkflowDigest,
		pins.GooberDigest, pins.ConfigGeneration, repo, pins.BranchNamespace, pins.InputDigest, f.handle.Epoch); err != nil {
		t.Fatal(err)
	}
	f.bindController(t)
	if f.grants, err = podauth.NewSignedKey(bytes.Repeat([]byte{9}, podauth.MinSignedKeyBytes)); err != nil {
		t.Fatal(err)
	}
	return f
}

// bindController binds the controller role's store to the current lease.
func (f *authorityFamily) bindController(t *testing.T) {
	t.Helper()
	controller, err := acceptance.New(controllerPool(t, f.pool), testInstance)
	if err != nil {
		t.Fatal(err)
	}
	controller.BindFence(f.manager, f.handle)
	controller.BindChildBlobs(f.blobs)
	f.controller = controller
}

func (f *authorityFamily) authorities() Authorities {
	parents := f.parent.authority()
	parents.Pins = f.controller.RunPinsOf
	return Authorities{Queue: f.controller, Parents: parents, Grants: f.grants, Now: time.Now}
}

func TestAuthoritiesBindRedeliverAndReleaseTheExactGrant(t *testing.T) {
	f := newAuthorityFamily(t)
	authorities := f.authorities()
	pins, origin := f.parent.pins, f.parent.origin
	grant, err := authorities.Acquire(t.Context(), pins.RunID, origin, podEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := f.grants.VerifyChildWorkflowGrant(grant.BearerToken)
	if err != nil || grant.Endpoint != podEndpoint || verified.ID != grant.GrantID || verified.RunID != pins.RunID ||
		verified.StageOccurrence != origin.StageOccurrence || verified.AttemptID != origin.AttemptID {
		t.Fatalf("grant = %+v (%+v, %v)", grant, verified, err)
	}
	parent := triggerqueue.ChildParent{Gaggle: pins.Gaggle, ParentRunID: pins.RunID}
	bound, err := f.controller.ChildAuthority(t.Context(), parent, origin.StageOccurrence)
	if err != nil || bound.GrantID != grant.GrantID || bound.Revoked {
		t.Fatalf("bound authority = %+v, %v", bound, err)
	}
	if err := f.controller.CheckChildAuthority(t.Context(), bound, time.Now()); err != nil {
		t.Fatalf("bound grant does not check: %v", err)
	}

	// The worker's response was lost: the same live attempt gets the same
	// grant back, not a second one and not a refusal.
	again, err := authorities.Acquire(t.Context(), pins.RunID, origin, podEndpoint)
	if err != nil || again.GrantID != grant.GrantID || !again.ExpiresAt.Equal(grant.ExpiresAt) {
		t.Fatalf("redelivery = %+v, %v; want grant %s again", again, err, grant.GrantID)
	}

	// A release naming another grant leaves the binding live.
	if err := authorities.Release(t.Context(), pins.RunID, origin, "some-other-grant"); err != nil {
		t.Fatal(err)
	}
	if bound, err = f.controller.ChildAuthority(t.Context(), parent, origin.StageOccurrence); err != nil || bound.Revoked {
		t.Fatalf("a release of another grant revoked this one: %+v, %v", bound, err)
	}
	if err := authorities.Release(t.Context(), pins.RunID, origin, grant.GrantID); err != nil {
		t.Fatal(err)
	}
	if bound, err = f.controller.ChildAuthority(t.Context(), parent, origin.StageOccurrence); err != nil || !bound.Revoked {
		t.Fatalf("released grant still live: %+v, %v", bound, err)
	}
	if err := authorities.Release(t.Context(), pins.RunID, origin, grant.GrantID); err != nil {
		t.Fatalf("a repeated release = %v, want done", err)
	}
	// A revoked attempt is never revived.
	if _, err := authorities.Acquire(t.Context(), pins.RunID, origin, podEndpoint); !errors.Is(err, engine.ErrChildCustodyRefused) {
		t.Fatalf("acquire after release = %v, want a refusal", err)
	}
}

// A release on custody an earlier epoch still owns must be retried after
// adoption; reporting it released would leave the grant live.
func TestAuthoritiesReleaseOnUnadoptedCustodyIsRetried(t *testing.T) {
	f := newAuthorityFamily(t)
	pins, origin := f.parent.pins, f.parent.origin
	grant, err := f.authorities().Acquire(t.Context(), pins.RunID, origin, podEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.manager.Release(t.Context(), f.handle); err != nil {
		t.Fatal(err)
	}
	f.takeOver(t, "controller-2")
	f.bindController(t)
	if err := f.authorities().Release(t.Context(), pins.RunID, origin, grant.GrantID); !errors.Is(err, engine.ErrChildCustodyUnavailable) {
		t.Fatalf("release on unadopted custody = %v, want a retryable unavailability", err)
	}
	if _, err := f.controller.AdoptStaleChildCustody(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.authorities().Release(t.Context(), pins.RunID, origin, grant.GrantID); err != nil {
		t.Fatalf("release after adoption: %v", err)
	}
	bound, err := f.controller.ChildAuthority(t.Context(), triggerqueue.ChildParent{Gaggle: pins.Gaggle, ParentRunID: pins.RunID}, origin.StageOccurrence)
	if err != nil || !bound.Revoked {
		t.Fatalf("grant still live after the retried release: %+v, %v", bound, err)
	}
}

func TestAuthoritiesReleaseOfAnUnknownRunOrUnboundStageIsDone(t *testing.T) {
	f := newAuthorityFamily(t)
	if err := f.authorities().Release(t.Context(), "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", f.parent.origin, "grant"); err != nil {
		t.Fatalf("release for an unpinned run = %v", err)
	}
	if err := f.authorities().Release(t.Context(), f.parent.pins.RunID, f.parent.origin, "grant"); err != nil {
		t.Fatalf("release with nothing bound = %v", err)
	}
}
