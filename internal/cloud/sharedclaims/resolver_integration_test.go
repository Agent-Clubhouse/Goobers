//go:build cloudintegration

package sharedclaims

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/cloud/journalpg"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/journal"
)

func TestSharedPolicyReadsPGWorkflowAndControlPinAsController(t *testing.T) {
	owner := pgtest.Pool(t, "sharedpolicy")
	controller := pgtest.ControllerPool(t, owner)
	manager := pgtest.Manager(t, controller, "leader", 0)
	handle := pgtest.Acquire(t, manager)
	backend, err := journalpg.New(journalpg.Config{Pool: controller, InstanceID: pgtest.InstanceID, Lease: manager, LiveOwner: func() *lease.Handle { return handle }})
	if err != nil {
		t.Fatal(err)
	}
	fixture, _, _ := resolverFixture(t, "shared")
	reader, err := fixture.Journal.OpenRun(t.Context(), journal.RunRef{ID: "run-a"})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	identity.ConfigGeneration = "generation"
	definition, err := reader.ArtifactBytes(identity.Inputs[0].Ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := backend.CreateRun(t.Context(), journal.CreateRunRequest{Identity: identity, Inputs: map[string][]byte{journal.PinnedWorkflowDefinitionInputName: definition}}); err != nil {
		t.Fatal(err)
	}
	store, err := acceptance.New(controller, pgtest.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	resolver := Resolver{InstanceID: pgtest.InstanceID, Journal: backend, Verify: ControlPins(store.RunPinsOf)}
	if _, err := resolver.Visibility(t.Context(), identity.RunID); !errors.Is(err, ErrPolicyPinUnavailable) {
		t.Fatalf("missing own pin=%v", err)
	}
	repo, err := json.Marshal(identity.WorkspaceRepository)
	if err != nil {
		t.Fatal(err)
	}
	// Fixture pin insertion still uses runtime privileges and the real lease fence.
	err = manager.WithFence(t.Context(), handle, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO control.run_pin(instance_id,run_id,acceptance_id,gaggle,workflow,workflow_version,
			workflow_digest,goober_digest,config_generation,repo_ref,branch_namespace,input_digest,owner_epoch)
			VALUES($1,$2,'acceptance',$3,$4,$5,$6,'goober',$7,$8,'','input',$9)`, pgtest.InstanceID, identity.RunID, identity.Gaggle,
			identity.Workflow, identity.WorkflowVersion, identity.WorkflowDigest, identity.ConfigGeneration, repo, handle.Epoch)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if mode, err := resolver.Visibility(t.Context(), identity.RunID); err != nil || mode != "shared" {
		t.Fatalf("PG snapshot policy=%q, %v", mode, err)
	}
	// A journaled self-consistent replacement is not enough: the control pin wins.
	if _, err := owner.Exec(t.Context(), `UPDATE control.run_pin SET workflow_digest='sha256:different' WHERE instance_id=$1`, pgtest.InstanceID); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Visibility(t.Context(), identity.RunID); err == nil {
		t.Fatal("workflow pin disagreement accepted")
	}
}
