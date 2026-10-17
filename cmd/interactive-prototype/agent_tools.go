package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/goobers/goobers/internal/interactive"
	"github.com/goobers/goobers/internal/interactiveagent"
	auth "github.com/goobers/goobers/internal/interactiveauth"
	"github.com/goobers/goobers/internal/interactiveexec"
	"github.com/goobers/goobers/internal/interactiveops"
	"github.com/goobers/goobers/internal/secretpattern"
)

// This host adapter is deliberately narrow. No tool can choose an actor,
// session, authority, URL, filesystem root, or approval identity. Effectful
// management tools only create immutable proposals for a human decision.
type agentToolHost struct {
	sessions *interactive.Service
	ops      *interactiveops.Service
	executor *interactiveexec.Executor
	catalogs *catalogStore
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}

func modelTools() []interactiveagent.Tool {
	str := func(limit int) map[string]any { return map[string]any{"type": "string", "maxLength": limit} }
	key := map[string]any{"type": "string", "pattern": "^[a-zA-Z0-9_-]{1,64}$"}
	request := objectSchema(map[string]any{
		"kind":   map[string]any{"type": "string", "enum": []string{"add_workflow", "tweak_workflow", "tweak_gaggle", "submit_run", "stop_run"}},
		"gaggle": str(64), "workflow": str(64), "displayName": str(200), "enabled": map[string]any{"type": "boolean"}, "workflowYaml": str(32768), "runId": str(128),
	}, "kind")
	return []interactiveagent.Tool{
		{Name: "session_context", Description: "Read the actual configuration, workflows, native run/action receipts and labeled fixture backlog for this private session. Use before planning management work. Scope is fixed by the host.", InputSchema: objectSchema(map[string]any{})},
		{Name: "inspect_action", Description: "Read an owned proposal or observed native execution receipt. A queued/running/unknown result is not completion.", InputSchema: objectSchema(map[string]any{"actionId": str(128)}, "actionId")},
		{Name: "validate_workflow", Description: "Validate authored Workflow DSL before proposing it. Use the configured gaggle, portable repository-relative node --test paths (*.test.mjs) or node --check (*.mjs), manual trigger, 1–8 deterministic stages, scratch workspace, and 1–30 second stage deadlines. DSL 3.0 requires runsOn.restrictions: [network:none] and its own preview acknowledgement. Returns diagnostics and normalized source; no workflow is applied.", InputSchema: objectSchema(map[string]any{"workflowYaml": str(32768)}, "workflowYaml")},
		{Name: "propose_action", Description: "Propose an exact management change or native workflow submission/cancellation for human review in Actions. No effect is executed by this tool. add_workflow accepts your authored Workflow DSL. Reuse key only for an unchanged intent. Human Apply records approval; no model approval tool exists.", InputSchema: objectSchema(map[string]any{"key": key, "request": request}, "key", "request")},
		{Name: "propose_child_workflow", Description: "Validate and propose your authored bounded workflow for an isolated child snapshot. No child starts until the human reviews this exact definition and approves it at an idle parent checkpoint. Reuse key for unchanged intent. Finish the current turn after presenting pending proposals.", InputSchema: objectSchema(map[string]any{"key": key, "prompt": str(4096), "workflowYaml": str(32768)}, "key", "prompt", "workflowYaml")},
		{Name: "read_work_item", Description: "Read an ADO-shaped item through the current scoped delegated-authority broker. This checkpoint has controlled local item427 only, not live ADO. Does not accept resource/identity/token overrides.", InputSchema: objectSchema(map[string]any{"key": key, "workItemId": map[string]any{"type": "integer", "minimum": 1}}, "key", "workItemId")},
		{Name: "query_telemetry", Description: "Read controlled local App Insights through current scoped authority. Supported query: requests | summarize count() by resultCode. This is not a live Azure API.", InputSchema: objectSchema(map[string]any{"key": key, "query": str(4096)}, "key", "query")},
	}
}

func decodeToolArguments(raw json.RawMessage, target any) error {
	if len(raw) > 64<<10 {
		return fmt.Errorf("tool arguments exceed 64 KiB")
	}
	if !bytes.Equal(secretpattern.NewScrubber().Scrub(raw), raw) {
		return fmt.Errorf("tool arguments must not contain credentials")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return fmt.Errorf("invalid typed tool arguments: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("tool requires one JSON object")
	}
	return nil
}

var toolKey = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func agentIntentKey(turnID, key string) (string, error) {
	if !toolKey.MatchString(key) {
		return "", fmt.Errorf("key requires 1–64 letters, digits, underscores or hyphens")
	}
	sum := sha256.Sum256([]byte(turnID + "\x00" + key))
	return "agent_" + hex.EncodeToString(sum[:]), nil
}

func (h agentToolHost) options(req interactive.ExecutionRequest, emit func(string, map[string]any) error) interactiveagent.Options {
	scope := interactiveops.Scope{ActorID: req.Owner, SessionID: req.SessionID}
	allowedTools := []interactiveagent.Tool{}
	for _, tool := range modelTools() {
		if slices.Contains(req.Tools, tool.Name) {
			allowedTools = append(allowedTools, tool)
		}
	}
	check := func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if req.ChildID != "" || req.ReadOnly {
			return fmt.Errorf("review child cannot access parent management tools")
		}
		if h.catalogs != nil {
			if _, _, err := h.catalogs.execution(req); err != nil {
				return err
			}
		}
		return h.sessions.CheckExecution(req.Owner, req.SessionID, req.TurnID, req.LeaseID, req.Fence)
	}
	return interactiveagent.Options{Tools: allowedTools, Check: check, Invoke: func(ctx context.Context, call interactiveagent.Call) (value any, invokeErr error) {
		if err := check(ctx); err != nil {
			return nil, err
		}
		if !slices.Contains(req.Tools, call.Name) {
			return nil, fmt.Errorf("tool is not allowed by this surface's DSL policy")
		}
		data := map[string]any{"name": "agent." + call.Name, "actorSource": "model-tool", "executionMode": "codex-local"}
		if err := emit("tool.started", data); err != nil {
			return nil, err
		}
		defer func() {
			finished := map[string]any{"name": "agent." + call.Name, "actorSource": "model-tool", "status": "succeeded", "result": value}
			if action, ok := value.(interactiveops.Action); ok {
				finished["actionId"], finished["status"] = action.ID, action.Status
			}
			if receipt, ok := value.(interactive.CommandReceipt); ok {
				finished["childId"], finished["status"] = receipt.ChildID, "proposed"
			}
			if invokeErr != nil {
				finished["status"], finished["error"] = "failed", string(secretpattern.NewScrubber().Scrub([]byte(invokeErr.Error())))
			}
			if err := emit("tool.completed", finished); invokeErr == nil && err != nil {
				invokeErr = err // Outcome may be committed; caller must retain its stable key.
			}
		}()
		switch call.Name {
		case "session_context":
			if err := decodeToolArguments(call.Arguments, &struct{}{}); err != nil {
				return nil, err
			}
			return h.ops.Snapshot(ctx, scope)
		case "inspect_action":
			var args struct {
				ActionID string `json:"actionId"`
			}
			if err := decodeToolArguments(call.Arguments, &args); err != nil {
				return nil, err
			}
			return h.ops.Get(ctx, scope, args.ActionID)
		case "validate_workflow":
			var args struct {
				YAML string `json:"workflowYaml"`
			}
			if err := decodeToolArguments(call.Arguments, &args); err != nil {
				return nil, err
			}
			return h.ops.ValidateWorkflow(ctx, scope, args.YAML)
		case "propose_action":
			var args struct {
				Key     string                 `json:"key"`
				Request interactiveops.Request `json:"request"`
			}
			if err := decodeToolArguments(call.Arguments, &args); err != nil {
				return nil, err
			}
			if args.Request.IdempotencyKey != "" || args.Request.Message != "" {
				return nil, fmt.Errorf("host owns idempotency and audit metadata")
			}
			switch args.Request.Kind {
			case "add_workflow", "tweak_workflow", "tweak_gaggle", "submit_run", "stop_run":
			default:
				return nil, fmt.Errorf("management operation is outside model proposal tools")
			}
			key, err := agentIntentKey(req.TurnID, args.Key)
			if err != nil {
				return nil, err
			}
			args.Request.IdempotencyKey, args.Request.Message = key, "Model proposal from turn "+req.TurnID
			return h.ops.Propose(ctx, scope, args.Request)
		case "propose_child_workflow":
			var args struct {
				Key    string `json:"key"`
				Prompt string `json:"prompt"`
				YAML   string `json:"workflowYaml"`
			}
			if err := decodeToolArguments(call.Arguments, &args); err != nil {
				return nil, err
			}
			key, err := agentIntentKey(req.TurnID, args.Key)
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(args.YAML) == "" || strings.TrimSpace(args.Prompt) == "" {
				return nil, fmt.Errorf("child proposal requires a description and authored workflow")
			}
			validation, err := h.ops.ValidateWorkflow(ctx, scope, args.YAML)
			if err != nil {
				return nil, err
			}
			if !validation.Valid {
				return validation, nil
			}
			return h.sessions.RequestChildFromExecution(ctx, req.Owner, req.SessionID, req.TurnID, req.LeaseID, req.Fence, key, args.Prompt, validation.NormalizedYAML)
		case "read_work_item":
			var args struct {
				Key        string `json:"key"`
				WorkItemID int    `json:"workItemId"`
			}
			if err := decodeToolArguments(call.Arguments, &args); err != nil {
				return nil, err
			}
			return h.executor.ReadProvider(ctx, req, args.Key, auth.ReadWorkItem, auth.Payload{WorkItemID: args.WorkItemID})
		case "query_telemetry":
			var args struct {
				Key   string `json:"key"`
				Query string `json:"query"`
			}
			if err := decodeToolArguments(call.Arguments, &args); err != nil {
				return nil, err
			}
			return h.executor.ReadProvider(ctx, req, args.Key, auth.QueryTelemetry, auth.Payload{Query: args.Query})
		default:
			return nil, fmt.Errorf("unknown model tool")
		}
	}}
}
