//go:build integration && (linux || darwin)

package interactiveops

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationNativeWorkflowExecution(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_INTERACTIVE_TEST_BINARY")
	testdep.Require(t, "node")
	binary := os.Getenv("GOOBERS_INTERACTIVE_TEST_BINARY")
	svc, scope := fixture(t, binary)
	ctx := context.Background()
	state, err := svc.Snapshot(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	a, err := svc.Propose(ctx, scope, Request{Kind: "add_workflow", WorkflowYAML: state.WorkflowDraft, IdempotencyKey: "author"})
	if err != nil {
		t.Fatal(err)
	}
	a, err = svc.Apply(ctx, scope, a.ID)
	if err != nil || a.Status != "succeeded" {
		t.Fatalf("author: %+v %v", a, err)
	}
	run := func(key string) Action {
		t.Helper()
		a, err := svc.Propose(ctx, scope, Request{Kind: "submit_run", Workflow: "invoice-check-custom", IdempotencyKey: key})
		if err != nil {
			t.Fatal(err)
		}
		a, err = svc.Apply(ctx, scope, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(70 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
			current, getErr := svc.Get(ctx, scope, a.ID)
			if getErr != nil {
				err = getErr
				continue
			}
			a = current
			err = nil
			if a.Status != "running" && a.Status != "admitted" && (a.CleanupConfirmed || a.OperationStatus == "failed") {
				return a
			}
		}
		t.Fatalf("native execution timed out: %+v; last error %v", a, err)
		return a
	}
	failed := run("failing-test")
	if failed.Status != "failed" || failed.RunID == "" || !strings.Contains(failed.Output, "discount floor") {
		data, _ := json.MarshalIndent(failed, "", "  ")
		t.Fatalf("actual failing test missing: %s", data)
	}
	if err = os.WriteFile(filepath.Join(state.Repository, "invoice.mjs"), []byte("export function invoiceTotal(subtotal,discount=0){return Math.max(0,subtotal-discount)}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	passed := run("passing-test")
	if passed.Status != "succeeded" || passed.RunID == "" || !strings.Contains(passed.Output, "discount floor") {
		data, _ := json.MarshalIndent(passed, "", "  ")
		t.Fatalf("actual passing test missing: %s", data)
	}
	state, err = svc.Snapshot(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Runs) != 2 {
		t.Fatalf("native runs: %+v", state.Runs)
	}
	t.Logf("real native scheduler/runner: failed run %s, corrected-workspace passing run %s", failed.RunID, passed.RunID)
}

func TestIntegrationNativeStopFencesNewOperations(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_INTERACTIVE_TEST_BINARY")
	testdep.Require(t, "node")
	binary := os.Getenv("GOOBERS_INTERACTIVE_TEST_BINARY")
	svc, scope := fixture(t, binary)
	ctx := context.Background()
	state, err := svc.Snapshot(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(state.Repository, "invoice.test.mjs"), []byte("import test from 'node:test';test('long running test', async()=>{await new Promise(r=>setTimeout(r,20000));});\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := svc.Propose(ctx, scope, Request{Kind: "submit_run", Workflow: "invoice-check", IdempotencyKey: "long-run"})
	if err != nil {
		t.Fatal(err)
	}
	a, err = svc.Apply(ctx, scope, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	started := false
	for time.Now().Before(deadline) {
		current, err := svc.Get(ctx, scope, a.ID)
		if err == nil && strings.Contains(current.Output, "stage test started") {
			started = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !started {
		t.Fatal("native stage did not start")
	}
	stopCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err = svc.StopSession(stopCtx, scope); err != nil {
		t.Fatal(err)
	}
	a, err = svc.Get(ctx, scope, a.ID)
	if err != nil || !a.CleanupConfirmed {
		t.Fatalf("cleanup not confirmed: %+v %v", a, err)
	}
	if _, err = svc.Propose(ctx, scope, Request{Kind: "submit_run", Workflow: "invoice-check", IdempotencyKey: "after-stop"}); err == nil {
		t.Fatal("stopped scope admitted new work")
	}
	if err = svc.Close(stopCtx); err != nil {
		t.Fatal(err)
	}
	t.Logf("native process-tree cleanup confirmed for %s; terminal journal remains %s when force-stopped", a.RunID, a.Status)
}

func TestIntegrationNativeReviewChildReplayAndReceipts(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_INTERACTIVE_TEST_BINARY")
	testdep.Require(t, "node")
	svc, parent := fixture(t, os.Getenv("GOOBERS_INTERACTIVE_TEST_BINARY"))
	ctx := context.Background()
	child := Scope{ActorID: parent.ActorID, SessionID: parent.SessionID + "--review"}
	state, err := svc.Snapshot(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(state.Repository, "invoice.mjs"), []byte("export function invoiceTotal(subtotal,discount=0){return Math.max(0,subtotal-discount)}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	prompt := "Review the invoice regression tests; descriptive text is never a command."
	var events []ReviewChildEvent
	result, err := svc.RunReviewChild(ctx, child, "review-request", prompt, func(e ReviewChildEvent) error { events = append(events, e); return nil })
	if err != nil || result.Status != "succeeded" || !result.CleanupConfirmed || result.RunID == "" || !strings.Contains(result.Output, "discount floor") {
		t.Fatalf("native review: %+v %v", result, err)
	}
	if len(events) < 3 || events[0].Kind != "proposal" || !strings.Contains(events[0].WorkflowYAML, "name: review-child") || strings.Contains(events[0].WorkflowYAML, prompt) {
		t.Fatalf("missing immutable test-only proposal: %+v", events)
	}
	replay, err := svc.RunReviewChild(ctx, child, "review-request", prompt, nil)
	if err != nil || replay.RunID != result.RunID {
		t.Fatalf("replay changed native effect: %+v %v", replay, err)
	}
	state, err = svc.Snapshot(ctx, child)
	if err != nil || len(state.Runs) != 1 {
		t.Fatalf("child replay duplicated run: %+v %v", state.Runs, err)
	}
	if _, err = svc.RunReviewChild(ctx, child, "review-request", "changed description", nil); err == nil {
		t.Fatal("request reused with changed intent")
	}
	parentState, err := svc.Snapshot(ctx, parent)
	if err != nil || len(parentState.Runs) != 0 || len(parentState.Workflows) != 1 {
		t.Fatalf("child changed parent: %+v %v", parentState, err)
	}
	t.Logf("native bounded child succeeded and replay reused run %s", result.RunID)
}

func TestIntegrationNativeReviewChildCancellationIsScoped(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_INTERACTIVE_TEST_BINARY")
	testdep.Require(t, "node")
	svc, parent := fixture(t, os.Getenv("GOOBERS_INTERACTIVE_TEST_BINARY"))
	child := Scope{ActorID: parent.ActorID, SessionID: parent.SessionID + "--cancel"}
	state, err := svc.Snapshot(context.Background(), child)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(state.Repository, "invoice.test.mjs"), []byte("import test from 'node:test';test('long child',async()=>{await new Promise(r=>setTimeout(r,20000));});\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	started := false
	result, err := svc.RunReviewChild(ctx, child, "cancel-request", "Run only bounded regression tests", func(e ReviewChildEvent) error {
		if e.Kind == "run" && e.Action != nil && strings.Contains(e.Action.Output, "stage test started") {
			started = true
			cancel()
		}
		return nil
	})
	if !started || err == nil || !result.CleanupConfirmed {
		t.Fatalf("scoped cancellation: started=%v result=%+v error=%v", started, result, err)
	}
	name := "Parent remains available"
	if _, err = svc.Propose(context.Background(), parent, Request{Kind: "tweak_gaggle", DisplayName: &name, IdempotencyKey: "parent-after-child-stop"}); err != nil {
		t.Fatalf("child cancellation fenced parent: %v", err)
	}
	t.Logf("cancelled child %s cleanup confirmed; parent scope remains available", result.RunID)
}

func TestIntegrationNativeDynamicChildExactDefinition(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_INTERACTIVE_TEST_BINARY")
	testdep.Require(t, "node")
	s, parent := customService(t, os.Getenv("GOOBERS_INTERACTIVE_TEST_BINARY"))
	ctx := context.Background()
	validation, err := s.ValidateWorkflow(ctx, parent, dynamicWorkflow)
	if err != nil || !validation.Valid {
		t.Fatalf("parent validation: %+v %v", validation, err)
	}
	child := Scope{ActorID: parent.ActorID, SessionID: "dynamic-child"}
	var definition ReviewChildEvent
	result, err := s.RunReviewChildWithOptions(ctx, child, "approved-dynamic-review", "Check syntax and both selected tests", ReviewChildOptions{WorkflowYAML: validation.NormalizedYAML, ApprovedPayloadDigest: validation.PayloadDigest}, func(event ReviewChildEvent) error {
		if event.Kind == "proposal" {
			definition = event
		}
		return nil
	})
	if err != nil || result.Status != "succeeded" || !result.CleanupConfirmed || result.RunID == "" {
		t.Fatalf("native dynamic child: %+v %v", result, err)
	}
	if definition.WorkflowYAML != validation.NormalizedYAML || definition.PayloadDigest != validation.PayloadDigest || definition.BindingDigest == validation.BindingDigest || !strings.Contains(result.Output, "dynamic arithmetic result") || !strings.Contains(result.Output, "dynamic second approved file") {
		t.Fatalf("exact dynamic payload/outputs missing: event=%+v result=%+v", definition, result)
	}
	replay, err := s.RunReviewChildWithOptions(ctx, child, "approved-dynamic-review", "Check syntax and both selected tests", ReviewChildOptions{WorkflowYAML: validation.NormalizedYAML, ApprovedPayloadDigest: validation.PayloadDigest}, nil)
	if err != nil || replay.RunID != result.RunID {
		t.Fatalf("dynamic replay: %+v %v", replay, err)
	}
	childState, err := s.Snapshot(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	if len(childState.Runs) != 1 {
		t.Fatalf("replay duplicated run: %+v", childState.Runs)
	}
	parentState, err := s.Snapshot(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(parentState.Actions) != 0 || len(parentState.Runs) != 0 {
		t.Fatal("child changed parent config/run ledger")
	}
	t.Logf("native two-stage exact child run=%s payload=%s binding=%s cleanup=%v", result.RunID, validation.PayloadDigest, definition.BindingDigest, result.CleanupConfirmed)
}

func TestIntegrationNativeDynamicSyntaxStopsBeforeRegression(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_INTERACTIVE_TEST_BINARY")
	testdep.Require(t, "node")
	s, scope := customService(t, os.Getenv("GOOBERS_INTERACTIVE_TEST_BINARY"))
	ctx := context.Background()
	state, err := s.Snapshot(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(state.Repository, "src/arithmetic.mjs"), []byte("export const = ;\n"), 0600); err != nil {
		t.Fatal(err)
	}
	validation, err := s.ValidateWorkflow(ctx, scope, dynamicWorkflow)
	if err != nil || !validation.Valid {
		t.Fatalf("validate: %+v %v", validation, err)
	}
	result, err := s.RunReviewChildWithOptions(ctx, scope, "bad-syntax", "Check syntax first", ReviewChildOptions{WorkflowYAML: validation.NormalizedYAML, ApprovedPayloadDigest: validation.PayloadDigest}, nil)
	if err != nil || result.Status != "failed" || !result.CleanupConfirmed || !strings.Contains(result.Output, "SyntaxError") || strings.Contains(result.Output, "dynamic arithmetic result") {
		t.Fatalf("real syntax stage did not stop chain: %+v %v", result, err)
	}
	t.Logf("native syntax failure run=%s cleanup=%v", result.RunID, result.CleanupConfirmed)
}

func TestIntegrationNativeDSL3ConfiguredScopeAndChild(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_INTERACTIVE_TEST_BINARY")
	testdep.Require(t, "node")
	s, parent, p := scopedNativeService(t, os.Getenv("GOOBERS_INTERACTIVE_TEST_BINARY"))
	basePolicy := *p
	s.opts.PolicyForScope = func(scope Scope) (Policy, error) {
		policy := basePolicy
		if scope.SessionID == "scoped-child" {
			policy.Workflows = []string{}
			policy.Revision = "attenuated-child"
		}
		return policy, nil
	}
	ctx := context.Background()
	state, err := s.Snapshot(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Workflows) != 1 || state.Workflows[0].DSLVersion != "3.0" {
		t.Fatal("configured DSL3 source was not selected")
	}
	validation, err := s.ValidateWorkflow(ctx, parent, dsl3Workflow("review-native-config", "team"))
	if err != nil || !validation.Valid {
		t.Fatalf("validate3: %+v %v", validation, err)
	}
	child := Scope{ActorID: parent.ActorID, SessionID: "scoped-child"}
	result, err := s.RunReviewChildWithOptions(ctx, child, "approved-dsl3", "Review the actual configured team workspace", ReviewChildOptions{WorkflowYAML: validation.NormalizedYAML, ApprovedPayloadDigest: validation.PayloadDigest}, nil)
	if err != nil || result.Status != "succeeded" || !result.CleanupConfirmed || result.Request.Gaggle != "team" || !strings.Contains(result.Output, "dynamic arithmetic result") {
		t.Fatalf("real scoped DSL3 child failed: %+v %v", result, err)
	}
	childState, err := s.Snapshot(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	if len(childState.Workflows) != 1 || childState.Workflows[0].Name != "review-native-config" || len(childState.Policy.Workflows) != 0 {
		t.Fatalf("attenuated child source broadened: %+v", childState)
	}
	cfg, err := instance.LoadConfig(filepath.Join(childState.InstanceRoot, "instance.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Repos) != 0 || !cfg.SelfRunnerEnforces(instance.RunnerRestrictionNetworkNone) {
		t.Fatal("private DSL3 runner must have no provider credentials and enforce network:none")
	}
	t.Logf("real DSL3 child run=%s gaggle=%s payload=%s cleanup=%v", result.RunID, result.Request.Gaggle, validation.PayloadDigest, result.CleanupConfirmed)
}

func TestIntegrationNativeDSL3StopAfterPolicyRevocation(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_INTERACTIVE_TEST_BINARY")
	testdep.Require(t, "node")
	s, scope, _ := scopedNativeService(t, os.Getenv("GOOBERS_INTERACTIVE_TEST_BINARY"))
	ctx := context.Background()
	state, err := s.Snapshot(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(state.Repository, "tests/arithmetic.test.mjs"), []byte("import test from 'node:test';test('owned live worker',async()=>{await new Promise(r=>setTimeout(r,18000));});\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := s.Propose(ctx, scope, Request{Kind: "submit_run", Workflow: "selected-check", IdempotencyKey: "live-before-revoke"})
	if err != nil {
		t.Fatal(err)
	}
	a, err = s.Apply(ctx, scope, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	started := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		current, getErr := s.Get(ctx, scope, a.ID)
		if getErr == nil && strings.Contains(current.Output, "stage regression started") {
			started = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !started {
		t.Fatal("native DSL3 regression stage did not start")
	}
	// Emulate the host's durable stop/reload fence: every live callback refuses.
	s.opts.PolicyForScope = func(Scope) (Policy, error) { return Policy{}, errors.New("surface stopping or retired") }
	s.opts.ConfigDirForScope = func(Scope) (string, error) { return "", errors.New("surface stopping or retired") }
	s.opts.RepositoryForScope = func(Scope) (string, error) { return "", errors.New("surface stopping or retired") }
	stopCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err = s.StopSession(stopCtx, scope); err != nil {
		t.Fatal(err)
	}
	receipt, err := s.Get(ctx, scope, a.ID)
	if err != nil || !receipt.CleanupConfirmed {
		t.Fatalf("revoked helper custody: %+v %v", receipt, err)
	}
	history, err := s.SnapshotRecorded(ctx, scope)
	if err != nil || len(history.Actions) != 1 || !history.Actions[0].CleanupConfirmed {
		t.Fatalf("retired history missing cleanup: %+v %v", history, err)
	}
	if err = s.Close(stopCtx); err != nil {
		t.Fatalf("shutdown after revocation failed: %v", err)
	}
	t.Logf("revoked native DSL3 worker run=%s cleanup=true historical-read=true", receipt.RunID)
}

func TestIntegrationNativeRunSnapshotIsolatesLaterParentEdits(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_INTERACTIVE_TEST_BINARY")
	testdep.Require(t, "node")
	s, scope, _ := scopedNativeService(t, os.Getenv("GOOBERS_INTERACTIVE_TEST_BINARY"))
	ctx := context.Background()
	state, err := s.Snapshot(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	originalTest := `import {test} from 'node:test'; import assert from 'node:assert/strict'; import {writeFileSync} from 'node:fs'; import {add} from '../src/arithmetic.mjs'; writeFileSync(new URL('./worker-write.txt',import.meta.url),'private execution write'); test('captured revision passed',()=>assert.equal(add(2,3),5));`
	if err = os.WriteFile(filepath.Join(state.Repository, "tests/arithmetic.test.mjs"), []byte(originalTest), 0600); err != nil {
		t.Fatal(err)
	}
	captured := t.TempDir()
	calls := 0
	s.opts.RunRepositoryForScope = func(_ context.Context, got Scope, key string) (string, error) {
		calls++
		if got != scope || key == "" {
			t.Fatal("snapshot callback lost action identity")
		}
		if err := copyTree(state.Repository, captured); err != nil {
			return "", err
		}
		// This mutation models a new chat turn after the host's idle capture fence.
		if err := os.WriteFile(filepath.Join(state.Repository, "src/arithmetic.mjs"), []byte("export const add=()=>0;\n"), 0600); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(state.Repository, "tests/arithmetic.test.mjs"), []byte("throw new Error('later parent revision must not execute');\n"), 0600); err != nil {
			return "", err
		}
		return captured, nil
	}
	before, err := configDigest(filepath.Join(state.InstanceRoot, "config"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Propose(ctx, scope, Request{Kind: "submit_run", Workflow: "selected-check", IdempotencyKey: "immutable-run"})
	if err != nil {
		t.Fatal(err)
	}
	a, err = s.Apply(ctx, scope, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		current, getErr := s.Get(ctx, scope, a.ID)
		if getErr == nil {
			a = current
			if a.CleanupConfirmed {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if a.Status != "succeeded" || !a.CleanupConfirmed || a.ExecutionRoot == "" || a.ExecutionRepository == state.Repository || a.ExecutionPayloadDigest == "" || !strings.Contains(a.Output, "captured revision passed") || strings.Contains(a.Output, "later parent revision must not execute") {
		t.Fatalf("run did not isolate capture: %+v", a)
	}
	if _, err = os.Stat(filepath.Join(captured, "tests/worker-write.txt")); err != nil {
		t.Fatalf("worker did not write its own captured tree: %v", err)
	}
	if _, err = os.Stat(filepath.Join(state.Repository, "tests/worker-write.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("test worker mutated parent repository")
	}
	after, err := configDigest(filepath.Join(state.InstanceRoot, "config"))
	if err != nil || before != after {
		t.Fatal("run snapshot mutated authoring config")
	}
	replay, err := s.Apply(ctx, scope, a.ID)
	if err != nil || replay.RunID != a.RunID || calls != 1 {
		t.Fatalf("replay recaptured workspace: %+v calls=%d %v", replay, calls, err)
	}
	history, err := s.SnapshotRecorded(ctx, scope)
	if err != nil || len(history.Runs) != 1 || history.Runs[0].ID != a.RunID {
		t.Fatalf("per-action native journal missing from history: %+v %v", history, err)
	}
	t.Logf("snapshot run=%s root=%s payload=%s cleanup=true parent-edits-isolated=true", a.RunID, a.ExecutionRoot, a.ExecutionPayloadDigest)
}
