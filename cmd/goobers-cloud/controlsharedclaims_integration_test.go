//go:build cloudintegration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/cloud/journalpg"
	"github.com/goobers/goobers/internal/cloud/lease"
	cloudpg "github.com/goobers/goobers/internal/cloud/pg"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/cloud/sharedclaims"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

func TestComposedClaimsRequireOwnPinAndReturnSharedExecutionMode(t *testing.T) {
	pool := pgtest.ControllerPool(t, pgtest.Pool(t, "sharedcomposition"))
	manager := pgtest.Manager(t, pool, "leader", 0)
	handle := pgtest.Acquire(t, manager)
	owner := func() *lease.Handle { return handle }
	backend, err := journalpg.New(journalpg.Config{Pool: pool, InstanceID: pgtest.InstanceID, Lease: manager, LiveOwner: owner})
	if err != nil {
		t.Fatal(err)
	}
	pins, err := acceptance.New(pool, pgtest.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	planes := claimProviderPlanes()
	planes.journalReader = backend
	db := databaseServices{Pools: &cloudpg.Pools{Transaction: pool, Direct: pool}, InstanceID: pgtest.InstanceID, Acceptances: pins}
	if err := constructClaims(t.Context(), db, controlAuth{}, manager, owner, &planes); err != nil {
		t.Fatal(err)
	}
	if planes.provider == nil || planes.sharedRegistry == nil {
		t.Fatal("shared provider or durable repository registry was not composed")
	}
	definition := workflow.Definition{Name: "claim", Version: 1, Spec: apiv1.WorkflowSpec{Gaggle: "example", Start: "claim",
		Readiness: apiv1.ReadinessConditions{ClaimVisibility: "shared"},
		Tasks:     []apiv1.Task{{Name: "claim", Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{Command: []string{"true"}}}}}}
	machine, err := workflow.Compile(definition, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(machine.Def)
	if err != nil {
		t.Fatal(err)
	}
	id := journal.RunIdentity{InstanceID: pgtest.InstanceID, RunID: strings.Repeat("a", 32), Gaggle: "example",
		Workflow: "claim", WorkflowVersion: 1, WorkflowDigest: machine.Digest(), ConfigGeneration: "generation",
		WorkspaceRepository: &apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "service"}}
	if _, _, err := backend.CreateRun(t.Context(), journal.CreateRunRequest{Identity: id, Inputs: map[string][]byte{journal.PinnedWorkflowDefinitionInputName: raw}}); err != nil {
		t.Fatal(err)
	}
	request := httpapi.ClaimListRequest{RunID: id.RunID, Scope: httpapi.ClaimListScopeRun, Execution: true, IncludeHistory: true}
	if _, err := planes.Claims.List(t.Context(), request); !errors.Is(err, sharedclaims.ErrPolicyPinUnavailable) {
		t.Fatalf("missing own policy pin=%v", err)
	}
	repo, err := json.Marshal(id.WorkspaceRepository)
	if err != nil {
		t.Fatal(err)
	}
	err = manager.WithFence(t.Context(), handle, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO control.run_pin(instance_id,run_id,acceptance_id,gaggle,workflow,workflow_version,
			workflow_digest,goober_digest,config_generation,repo_ref,branch_namespace,input_digest,owner_epoch)
			VALUES($1,$2,'acceptance',$3,$4,$5,$6,'goober',$7,$8,'','input',$9)`, pgtest.InstanceID, id.RunID, id.Gaggle,
			id.Workflow, id.WorkflowVersion, id.WorkflowDigest, id.ConfigGeneration, repo, handle.Epoch)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := planes.Claims.List(t.Context(), request)
	if err != nil || snapshot.ClaimVisibility != "shared" || snapshot.ObservedAt.IsZero() {
		t.Fatalf("composed trusted execution snapshot=%+v, %v", snapshot, err)
	}
}
