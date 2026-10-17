package interactiveops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

func fixture(t *testing.T, binary string) (*Service, Scope) {
	t.Helper()
	src := t.TempDir()
	files := map[string]string{"invoice.mjs": "export function invoiceTotal(subtotal,discount=0){return subtotal-discount}\n", "invoice.test.mjs": "import test from 'node:test';import assert from 'node:assert/strict';import {invoiceTotal} from './invoice.mjs';test('discount floor',()=>assert.equal(invoiceTotal(20,30),0));\n", "gaggle-context.json": `{"backlog":[{"id":427,"title":"Invoice total becomes negative with a large discount"}]}`}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(src, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	svc, err := New(Options{Root: t.TempDir(), Repository: src, Binary: binary})
	if err != nil {
		t.Fatal(err)
	}
	return svc, Scope{ActorID: "alice", SessionID: "session-a"}
}
func TestConfigLifecycleIsolationAndReplay(t *testing.T) {
	svc, scope := fixture(t, "")
	ctx := context.Background()
	state, err := svc.Snapshot(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Backlog) != 1 || state.Backlog[0].ID != 427 {
		t.Fatalf("backlog not grounded: %+v", state.Backlog)
	}
	req := Request{Kind: "add_workflow", IdempotencyKey: "draft-1", WorkflowYAML: state.WorkflowDraft}
	a, err := svc.Propose(ctx, scope, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := svc.Propose(ctx, scope, req)
	if err != nil || again.ID != a.ID {
		t.Fatalf("replay: %+v %v", again, err)
	}
	req.WorkflowYAML = strings.Replace(req.WorkflowYAML, "invoice-check-custom", "different", 1)
	if _, err = svc.Propose(ctx, scope, req); err == nil {
		t.Fatal("different payload reused key")
	}
	if _, err = svc.Get(ctx, Scope{ActorID: "bob", SessionID: scope.SessionID}, a.ID); err == nil {
		t.Fatal("other actor read action")
	}
	applied, err := svc.Apply(ctx, scope, a.ID)
	if err != nil || applied.Status != "succeeded" {
		t.Fatalf("apply: %+v %v", applied, err)
	}
	repeated, err := svc.Apply(ctx, scope, a.ID)
	if err != nil || repeated.UpdatedAt != applied.UpdatedAt {
		t.Fatalf("apply replay mutated receipt: %+v %v", repeated, err)
	}
	set, _, err := instance.LoadConfigDir(filepath.Join(state.InstanceRoot, "config"))
	if err != nil || len(set.Workflows) != 2 {
		t.Fatalf("native config: %+v %v", set, err)
	}
	disabled := false
	a, err = svc.Propose(ctx, scope, Request{Kind: "tweak_workflow", Workflow: "invoice-check", Enabled: &disabled, IdempotencyKey: "disable"})
	if err != nil {
		t.Fatal(err)
	}
	a, err = svc.Apply(ctx, scope, a.ID)
	if err != nil || a.Status != "succeeded" {
		t.Fatalf("disable: %+v %v", a, err)
	}
	other, err := svc.Snapshot(ctx, Scope{ActorID: "bob", SessionID: scope.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(other.Workflows) != 1 || other.Repository == state.Repository {
		t.Fatal("session workspace/config leaked across owners")
	}
}
func TestRejectUnsafeWorkflowAndStalePreview(t *testing.T) {
	svc, scope := fixture(t, "")
	ctx := context.Background()
	state, err := svc.Snapshot(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"network":    strings.Replace(state.WorkflowDraft, "network: none", "network: ''", 1),
		"command":    strings.Replace(state.WorkflowDraft, `"node","--test"`, `"sh","-c"`, 1),
		"capability": strings.Replace(state.WorkflowDraft, "      run:", "      capabilities: [repo:push]\n      run:", 1),
		"remote":     strings.Replace(state.WorkflowDraft, "gaggle: interactive", "gaggle: production", 1),
		"invalid":    strings.Replace(state.WorkflowDraft, "start: test", "start: missing", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.Propose(ctx, scope, Request{Kind: "add_workflow", WorkflowYAML: content, IdempotencyKey: name}); err == nil {
				t.Fatal("unsafe/invalid workflow accepted")
			}
		})
	}
	name1 := "first"
	name2 := "second"
	a, err := svc.Propose(ctx, scope, Request{Kind: "tweak_gaggle", DisplayName: &name1, IdempotencyKey: "first"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.Propose(ctx, scope, Request{Kind: "tweak_gaggle", DisplayName: &name2, IdempotencyKey: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if b, err = svc.Apply(ctx, scope, b.ID); err != nil || b.Status != "succeeded" {
		t.Fatalf("second apply: %+v %v", b, err)
	}
	if _, err = svc.Apply(ctx, scope, a.ID); err == nil {
		t.Fatal("stale preview applied")
	}
}
func TestUnknownDispatchDoesNotRetry(t *testing.T) {
	svc, scope := fixture(t, "")
	ctx := context.Background()
	state, err := svc.Snapshot(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	id := "op_" + strings.Repeat("a", 32)
	a := Action{ID: id, OperationID: id, ActorID: scope.ActorID, SessionID: scope.SessionID, Kind: "submit_run", Status: "running", OperationStatus: "dispatched", ConfigDigest: state.ConfigDigest, CreatedAt: now, UpdatedAt: now}
	if err = saveAction(svc.scopeDir(scope), &a); err != nil {
		t.Fatal(err)
	}
	recovered, err := svc.Get(ctx, scope, id)
	if err != nil || recovered.Status != "uncertain" {
		t.Fatalf("recovery: %+v %v", recovered, err)
	}
	again, err := svc.Apply(ctx, scope, id)
	if err != nil || again.Status != "uncertain" {
		t.Fatalf("unknown replay dispatched: %+v %v", again, err)
	}
}
