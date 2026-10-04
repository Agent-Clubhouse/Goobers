package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
	harnesstest "github.com/goobers/goobers/test/testsupport/harness"
)

func configuredChildStage(t *testing.T, f pinnedChildFixture) (*journal.Run, apiv1.InvocationEnvelope) {
	t.Helper()
	_, machine, err := childStageCatalog(f.cfg, f.applied, childworkflow.ParentSelection{
		Gaggle: f.parent.Gaggle, Workflow: f.parent.Workflow, Stage: "plan",
	}, childworkflow.BackendRunner)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := json.Marshal(machine.Def)
	if err != nil {
		t.Fatal(err)
	}
	identity := f.parent
	identity.RunID = strings.Repeat("b", 32)
	run, err := journal.Create(f.layout.ForGaggle(identity.Gaggle).RunsDir(), identity,
		map[string][]byte{journal.PinnedWorkflowDefinitionInputName: definition},
		journal.WithInputIntegrity(map[string]apiv1.Integrity{journal.PinnedWorkflowDefinitionInputName: apiv1.IntegrityTrusted}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	_, origin, err := run.AppendChildStageStarted(journal.Event{Type: journal.EventStageStarted, Stage: "plan", Attempt: 1,
		Runner: map[string]any{"goober": f.stage.Goober}}, false)
	if err != nil {
		t.Fatal(err)
	}
	return run, apiv1.InvocationEnvelope{TaskID: identity.RunID + ":plan", RunID: identity.RunID, InstanceID: identity.InstanceID,
		WorkflowID: identity.Workflow, Gaggle: identity.Gaggle, Goober: f.stage.Goober, GooberDigest: identity.GooberDigest,
		ConfigGeneration: identity.ConfigGeneration, ChildWorkflowOrigin: origin, Attempt: 1,
		Capabilities: f.stage.Capabilities, Goal: f.stage.Goal, Workspace: t.TempDir()}
}

func TestChildWorkflowConfiguredHarnessRegistersAndRevokesGrant(t *testing.T) {
	parent := strings.Replace(childValidationParent, "capabilities: [agent:model]", "capabilities: [agent:model, repo:read]", 1)
	parent = strings.Replace(parent, "allowedCapabilities: [agent:model, repo:push]", "allowedCapabilities: [repo:read]", 1)
	f := newPinnedChildFixture(t, func(root string) {
		parentPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
		if err := os.WriteFile(parentPath, []byte(parent), 0o644); err != nil {
			t.Fatal(err)
		}
		gooberPath := filepath.Join(root, "config", "gaggles", "example", "goobers", "coder", "goober.yaml")
		goober, err := os.ReadFile(gooberPath)
		if err != nil {
			t.Fatal(err)
		}
		goober = bytes.Replace(goober, []byte("  capabilities:\n"), []byte("  capabilities:\n    - repo:read\n"), 1)
		if err := os.WriteFile(gooberPath, goober, 0o644); err != nil {
			t.Fatal(err)
		}
	})
	_, env := configuredChildStage(t, f)
	t.Setenv("GOOBERS_GITHUB_TOKEN", "configured-child-test-repo-token")
	queue, err := triggerqueue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	shared, perRun := journal.NewRegistryScrubber(), journal.NewRegistryScrubber()
	service := newDaemonCredentialService(f.layout, f.cfg, nil, shared, nil).withStageGrants(f.layout.Root, "127.0.0.1:8080", false)
	t.Cleanup(func() { unregisterDaemonStageGrants(f.layout.Root, service) })
	if err := service.enableChildWorkflows(queue, f.applied); err != nil {
		t.Fatal(err)
	}
	var token string
	calls := 0
	previous := newAgenticAdapter
	newAgenticAdapter = func(string, map[string]string) harness.Adapter {
		return &harnesstest.FakeAdapter{Act: func(ctx context.Context, req harness.RunRequest) error {
			calls++
			if req.ChildWorkflows == nil {
				t.Fatal("configured runner omitted trusted child access")
			}
			token = req.ChildWorkflows.BearerToken
			for name, registry := range map[string]*journal.RegistryScrubber{"run": perRun, "shared": shared} {
				if bytes.Contains(registry.Scrub([]byte(token)), []byte(token)) {
					t.Fatalf("grant reached harness before %s scrubber registration", name)
				}
			}
			validation, err := service.children.HTTPService().ValidateChildWorkflow(ctx, token, env.RunID, []byte(childValidationProposal))
			if err != nil || !validation.Valid {
				t.Fatalf("child subset validation through configured service: valid=%t err=%v", validation.Valid, err)
			}
			started, err := service.children.HTTPService().StartChildWorkflow(ctx, token, env.RunID, "inspect", []byte(childValidationProposal))
			if err != nil || started.RunID == "" {
				t.Fatalf("configured service failed custody acceptance: %v", err)
			}
			return harnesstest.WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		}}
	}
	t.Cleanup(func() { newAgenticAdapter = previous })
	authority := f.load(t)
	instructions, err := loadGooberInstructions(f.layout.ConfigDir(), authority.Admission.Goobers)
	if err != nil {
		t.Fatal(err)
	}
	config, _, err := buildRunnerConfig(runnerCompositionInput{
		Layout: f.layout.ForGaggle(f.parent.Gaggle), Config: f.cfg,
		Goobers: authority.Admission.Goobers, SharedRegistry: shared, SandboxPosture: instance.SandboxDisabled,
		InstructionsByGoober: instructions,
	})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := config.NewAgentic(f.stage.Goober, runnerWiringHarnessRecorder{dir: env.Workspace}, perRun)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Invoke(t.Context(), env); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || token == "" {
		t.Fatal("configured harness never received a child grant")
	}
	if _, err := service.children.HTTPService().ChildWorkflowStatus(t.Context(), token, env.RunID, "inspect"); err == nil {
		t.Fatal("harness return did not revoke the stage grant")
	}
	if _, _, err := childWorkflowAccessFor(f.layout.Root, perRun)(t.Context(), env); err == nil {
		t.Fatal("completed harness attempt renewed its revoked grant")
	}
}
