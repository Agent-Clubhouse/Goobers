package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/worktree"
)

type childKitFixture struct {
	writer  childKitWriter
	attempt dispatcher.Attempt
	parent  pinnedChildFixture
	child   triggerqueue.ChildRecord
}

func newChildKitFixture(t *testing.T) childKitFixture {
	t.Helper()
	f := newPinnedChildFixture(t, func(root string) {
		parent := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
		writeFileContent(t, parent, strings.Replace(childValidationParent, "allowPRPublication: true", "allowPRPublication: false", 1))
		path := filepath.Join(root, "config", "gaggles", "example", "goobers", "coder", "goober.yaml")
		writeFileContent(t, path, strings.Replace(readFileContent(t, path), "harness: copilot", "harness: claude-code", 1))
	})
	_, parentEnv := configuredChildStage(t, f)
	queue, err := triggerqueue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	service := newDaemonCredentialService(f.layout, f.cfg, nil, journal.NewRegistryScrubber(), nil).withStageGrants(f.layout.Root, "127.0.0.1:8080", false)
	t.Cleanup(func() { unregisterDaemonStageGrants(f.layout.Root, service) })
	if err := service.enableChildWorkflows(queue, f.applied); err != nil {
		t.Fatal(err)
	}
	access, revoke, err := service.children.Acquire(t.Context(), parentEnv, journal.NewRegistryScrubber())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = revoke() })
	source := strings.Replace(childValidationProposal, "type: deterministic", "type: agentic\n      goober: coder\n      workspace: scratch\n      capabilities: [agent:model]", 1)
	source = strings.Replace(source, "      run: {command: [\"true\"]}\n", "", 1)
	accepted, err := service.children.HTTPService().StartChildWorkflow(t.Context(), access.BearerToken, parentEnv.RunID, "kit", []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	identity := triggerqueue.ChildIdentity{ChildParent: triggerqueue.ChildParent{Gaggle: parentEnv.Gaggle, ParentRunID: parentEnv.RunID}, StageOccurrence: parentEnv.ChildWorkflowOrigin.StageOccurrence, InvocationKey: "kit"}
	receipt, err := queue.ChildStart(t.Context(), identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.BeginDispatch(t.Context(), receipt.ID); err != nil {
		t.Fatal(err)
	}
	ref, err := (&durableTriggerService{queue: queue}).childReference(t.Context(), receipt)
	if err != nil {
		t.Fatal(err)
	}
	launcher := &queuedChildLauncher{layout: f.layout, queue: queue, authority: service.children}
	authority, release, err := launcher.acquire(t.Context(), ref.Envelope)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := childworkflow.ValidateRetainedStart(authority, ref.Envelope, []byte(source))
	release()
	if err != nil {
		t.Fatal(err)
	}
	set, _, err := loadConfigDirectory(f.retainedPath)
	if err != nil {
		t.Fatal(err)
	}
	set.Workflows = []apiv1.Workflow{*proposal.Workflow.DeepCopy()}
	instructions, skills, err := loadSnapshotGooberInputs(f.retainedPath, set)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &workerConfigSnapshot{digests: newGooberDigestIndex(f.cfg, set, instructions, skills)}
	gooberDigest, err := snapshot.gooberDigestFor(parentEnv.Gaggle, proposal.Workflow.Name)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := worktree.NewManager(filepath.Join(t.TempDir(), "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	driver, err := runner.New(runner.Config{Worktrees: manager, RunsDir: f.layout.ForGaggle(parentEnv.Gaggle).RunsDir(), ScratchDir: t.TempDir(), ConfigGeneration: ref.Envelope.ConfigGeneration, InstanceID: f.parent.InstanceID})
	if err != nil {
		t.Fatal(err)
	}
	ceiling := proposal.CredentialCeiling()
	_, err = driver.Start(t.Context(), runner.StartInput{RunID: accepted.RunID, Gaggle: parentEnv.Gaggle, Child: &ref.Lineage, Machine: proposal.Machine, GooberDigest: gooberDigest, ChildCredentials: &ceiling, OnJournalPublished: func() error { return errors.New("publication interrupted") }})
	if err == nil {
		t.Fatal("expected interrupted launch")
	}
	dir, err := f.layout.FindRunDir(accepted.RunID)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Recover the interrupted journal writer without resuming execution.
	recorder, _, err := journal.Recover(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	env := apiv1.InvocationEnvelope{RunID: id.RunID, Gaggle: id.Gaggle, WorkflowID: id.Workflow, ConfigGeneration: id.ConfigGeneration, GooberDigest: id.GooberDigest, Goober: "coder", TaskID: id.RunID + ":check", Attempt: 1, Capabilities: []string{"agent:model"}}
	return childKitFixture{writer: childKitWriter{service: service, identity: id, blobs: blobs, recorder: recorder}, attempt: dispatcher.Attempt{RunID: id.RunID, Gaggle: id.Gaggle, Workflow: id.Workflow, Stage: "check", Number: 1, Agentic: true, Envelope: &env}, parent: f, child: ref.Child}
}

func TestChildKitWriterUsesRealAcceptedSourceAndRetainedInstructions(t *testing.T) {
	f := newChildKitFixture(t)
	if err := os.WriteFile(f.parent.sourcePath, []byte("invalid: ["), 0o644); err != nil {
		t.Fatal(err)
	}
	digest, err := f.writer.WriteKit(t.Context(), f.attempt)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := f.writer.blobs.Get(t.Context(), digest)
	if err != nil {
		t.Fatal(err)
	}
	var kit agentickit.Kit
	if err := json.Unmarshal(raw, &kit); err != nil {
		t.Fatal(err)
	}
	if len(kit.Goobers) != 1 || kit.Goobers["coder"].Harness != apiv1.HarnessClaudeCode || kit.Instructions["coder"] == "" {
		t.Fatalf("wrong child kit: %+v", kit)
	}
	for _, grant := range kit.Grants {
		if grant.Capability != "agent:model" {
			t.Fatalf("credential escaped: %+v", grant)
		}
	}
	foreign := f.attempt
	foreign.Stage = "foreign"
	if _, err := f.writer.WriteKit(t.Context(), foreign); err == nil {
		t.Fatal("foreign stage received a kit")
	}
	if err := f.writer.service.childQueue.FenceChildParent(t.Context(), f.child.Identity.ChildParent, "operator", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.writer.WriteKit(context.Background(), f.attempt); err == nil {
		t.Fatal("cancelled family published another kit")
	}
}
