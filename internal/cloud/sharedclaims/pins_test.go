package sharedclaims

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/journal"
)

func TestSharedSnapshotRefusesChildWithoutItsOwnPolicyPin(t *testing.T) {
	resolver, _, _ := resolverFixture(t, "shared")
	resolver.Verify = ControlPins(func(context.Context, string) (acceptance.RunPins, error) {
		return acceptance.RunPins{}, sql.ErrNoRows
	})
	if mode, err := resolver.Visibility(t.Context(), "run-a"); !errors.Is(err, ErrPolicyPinUnavailable) || mode != "" {
		t.Fatalf("child shared policy = %q, %v; want named missing-own-pin refusal", mode, err)
	}
}

func TestControlPinRejectsAConsistentButSubstitutedLocalSnapshot(t *testing.T) {
	local, _, _ := resolverFixture(t, "local")
	shared, _, _ := resolverFixture(t, "shared")
	reader, err := shared.Journal.OpenRun(t.Context(), journal.RunRef{ID: "run-a"})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	local.Verify = ControlPins(func(context.Context, string) (acceptance.RunPins, error) {
		return acceptance.RunPins{RunID: identity.RunID, Gaggle: identity.Gaggle, Workflow: identity.Workflow,
			WorkflowVersion: identity.WorkflowVersion, WorkflowDigest: identity.WorkflowDigest, ConfigGeneration: "generation",
			RepoRef: *identity.WorkspaceRepository}, nil
	})
	if mode, err := local.Visibility(t.Context(), "run-a"); err == nil || mode != "" {
		t.Fatalf("substituted policy=%q, %v", mode, err)
	}
}

func TestControlPinsMatchWorkflowAndKeepReadErrorsRetryable(t *testing.T) {
	id := journal.RunIdentity{RunID: "run-a", Gaggle: "example", Workflow: "claim", WorkflowVersion: 1,
		WorkflowDigest: "sha256:pinned", ConfigGeneration: "generation"}
	pin := acceptance.RunPins{RunID: id.RunID, Gaggle: id.Gaggle, Workflow: id.Workflow, WorkflowVersion: id.WorkflowVersion,
		WorkflowDigest: id.WorkflowDigest, ConfigGeneration: id.ConfigGeneration}
	verify := ControlPins(func(context.Context, string) (acceptance.RunPins, error) { return pin, nil })
	if err := verify(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	id.WorkflowDigest = "sha256:replacement"
	if err := verify(t.Context(), id); err == nil {
		t.Fatal("substituted digest accepted")
	}
	failed := errors.New("database unavailable")
	verify = ControlPins(func(context.Context, string) (acceptance.RunPins, error) { return acceptance.RunPins{}, failed })
	if err := verify(t.Context(), id); !errors.Is(err, failed) || errors.Is(err, ErrPolicyPinUnavailable) {
		t.Fatalf("read failure=%v", err)
	}
}
