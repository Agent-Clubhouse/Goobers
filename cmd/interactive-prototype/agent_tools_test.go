package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/interactive"
	"github.com/goobers/goobers/internal/interactiveagent"
	"github.com/goobers/goobers/internal/interactiveops"
)

type heldExecution struct {
	started chan interactive.ExecutionRequest
}

func (e *heldExecution) Execute(ctx context.Context, req interactive.ExecutionRequest, _ func(interactive.ExecutionEvent) error) (interactive.ExecutionResult, error) {
	e.started <- req
	<-ctx.Done()
	return interactive.ExecutionResult{}, ctx.Err()
}

func TestModelToolsProposeExactActionsAndChildrenWithoutSelfApproval(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	for name, content := range map[string]string{"invoice.mjs": "export const amount = 1;", "invoice.test.mjs": "import test from 'node:test'; test('amount',()=>{});"} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ops, err := interactiveops.New(interactiveops.Options{Root: t.TempDir(), Repository: source})
	if err != nil {
		t.Fatal(err)
	}
	executor := &heldExecution{started: make(chan interactive.ExecutionRequest, 1)}
	names := []string{}
	for _, tool := range modelTools() {
		names = append(names, tool.Name)
	}
	sessions, err := interactive.Open(interactive.Options{StateDir: t.TempDir(), Principal: func(*http.Request) (string, error) { return "alice", nil }, Executor: executor,
		AvailableExecutionModes: []string{"codex-local"}, DefaultExecutionMode: "codex-local", Surfaces: []interactive.Surface{{ID: "helper", Name: "Helper", Mode: "helper", Enabled: true, MaxChildren: 2, Tools: names}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sessions.Close(); err != nil {
			t.Error(err)
		}
	})
	session, err := sessions.Create("alice", interactive.CreateRequest{CommandID: "create", SurfaceID: "helper", ExecutionMode: "codex-local"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = sessions.Command(ctx, "alice", session.ID, interactive.Command{CommandID: "turn", Type: "message", Text: "Investigate the configuration and propose a useful two-stage verification workflow.", ExpectedFence: session.Fence, ExpectedSequence: session.Sequence})
	if err != nil {
		t.Fatal(err)
	}
	var req interactive.ExecutionRequest
	select {
	case req = <-executor.started:
	case <-time.After(3 * time.Second):
		t.Fatal("controlled execution did not start")
	}
	host := agentToolHost{sessions: sessions, ops: ops}
	options := host.options(req, func(string, map[string]any) error { return nil })
	call := func(name string, args any) (any, error) {
		raw, _ := json.Marshal(args)
		return options.Invoke(ctx, interactiveagent.Call{Name: name, Arguments: raw})
	}
	for _, tool := range options.Tools {
		if strings.Contains(tool.Name, "apply") || strings.Contains(tool.Name, "approve") {
			t.Fatalf("model can self approve: %s", tool.Name)
		}
	}
	value, err := call("session_context", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	state := value.(interactiveops.State)
	proposal := map[string]any{"key": "workflow-plan", "request": map[string]any{"kind": "add_workflow", "workflowYaml": state.WorkflowDraft}}
	value, err = call("propose_action", proposal)
	if err != nil {
		t.Fatal(err)
	}
	action := value.(interactiveops.Action)
	if action.Status != "proposed" || action.AuthorizedBy != "" {
		t.Fatalf("model proposal performed an effect: %+v", action)
	}
	value, err = call("propose_action", proposal)
	if err != nil || value.(interactiveops.Action).ID != action.ID {
		t.Fatalf("stable intent replay duplicated proposal: %v", err)
	}
	proposal["request"].(map[string]any)["workflowYaml"] = strings.Replace(state.WorkflowDraft, "invoice-check-custom", "different-proposal", 1)
	if _, err = call("propose_action", proposal); err == nil {
		t.Fatal("changed proposal reused stable key")
	}
	value, err = call("propose_child_workflow", map[string]any{"key": "child-plan", "prompt": "Run the reviewed verification in a private child snapshot.", "workflowYaml": state.WorkflowDraft})
	if err != nil {
		t.Fatal(err)
	}
	receipt := value.(interactive.CommandReceipt)
	current, err := sessions.Session("alice", session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(current.Children) != 1 || current.Children[0].ID != receipt.ChildID || current.Children[0].Status != "proposed" || current.Children[0].WorkflowDigest == "" {
		t.Fatalf("child was not an exact pending proposal: %+v", current.Children)
	}
	if _, err = call("session_context", map[string]any{"owner": "bob"}); err == nil {
		t.Fatal("model supplied caller identity")
	}
	if _, err = call("approve_action", map[string]any{"actionId": action.ID}); err == nil {
		t.Fatal("undeclared approval tool accepted")
	}
	forged := req
	forged.Owner = "bob"
	if err = host.options(forged, func(string, map[string]any) error { return nil }).Check(ctx); err == nil {
		t.Fatal("foreign owner passed host lease check")
	}
	current, _ = sessions.Session("alice", session.ID)
	_, err = sessions.Command(ctx, "alice", session.ID, interactive.Command{CommandID: "stop", Type: "stop", ExpectedFence: current.Fence, ExpectedSequence: current.Sequence})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = call("propose_action", map[string]any{"key": "after-stop", "request": map[string]any{"kind": "add_workflow", "workflowYaml": state.WorkflowDraft}}); err == nil {
		t.Fatal("stopped invocation retained management authority")
	}
}

func TestModelToolPolicyIsDefaultDeny(t *testing.T) {
	options := (agentToolHost{}).options(interactive.ExecutionRequest{}, func(string, map[string]any) error { return nil })
	if len(options.Tools) != 0 {
		t.Fatal("omitted mediated tools enabled defaults")
	}
	var value struct {
		Key string `json:"key"`
	}
	for _, raw := range []string{`{"key":"one","sessionId":"foreign"}`, `{"key":"one"} {"key":"two"}`} {
		if err := decodeToolArguments(json.RawMessage(raw), &value); err == nil {
			t.Fatalf("accepted unbound arguments: %s", raw)
		}
	}
}

func TestProductionModelToolCatalogCompilesOnPrivateBridge(t *testing.T) {
	bridge, err := interactiveagent.Start(context.Background(), interactiveagent.Options{
		Tools: modelTools(), Check: func(context.Context) error { return nil },
		Invoke: func(context.Context, interactiveagent.Call) (any, error) { return nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bridge.ToolNames()) != 7 {
		t.Fatal("incomplete production MCP catalog")
	}
	if err := bridge.Close(); err != nil {
		t.Fatal(err)
	}
}
