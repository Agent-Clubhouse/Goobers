//go:build integration && (linux || darwin)

package interactiveops

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNativeWorkflowExecution(t *testing.T) {
	binary := os.Getenv("GOOBERS_INTERACTIVE_TEST_BINARY")
	if binary == "" {
		t.Skip("set GOOBERS_INTERACTIVE_TEST_BINARY to built cmd/goobers for real scheduler/runner acceptance")
	}
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

func TestNativeStopFencesNewOperations(t *testing.T) {
	binary := os.Getenv("GOOBERS_INTERACTIVE_TEST_BINARY")
	if binary == "" {
		t.Skip("set GOOBERS_INTERACTIVE_TEST_BINARY for native process stop acceptance")
	}
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
