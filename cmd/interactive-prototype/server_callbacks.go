package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/interactive"
	"github.com/goobers/goobers/internal/interactiveagent"
	auth "github.com/goobers/goobers/internal/interactiveauth"
	"github.com/goobers/goobers/internal/interactiveops"
	"github.com/goobers/goobers/internal/invoke"
)

// prototypeHost binds the callbacks to the same services for one server lifetime.
// The session service is assigned when interactive.Open returns; callbacks keep
// the startup availability checks used before that assignment.
type prototypeHost struct {
	config     prototypeConfig
	catalogs   *catalogStore
	human      *humanAuth
	actors     []string
	service    *interactive.Service
	broker     *auth.Broker
	principals map[string]auth.Principal
	pool       *executors
	operations *interactiveops.Service
}

func (h *prototypeHost) checkExecution(ctx context.Context, identity auth.Identity, request auth.OperationRequest) error {
	if h.service == nil {
		return fmt.Errorf("session authority unavailable")
	}
	return h.service.CheckExecution(identity.Subject, request.SessionID, request.TurnID, request.LeaseID, request.ExecutionFence)
}

func (h *prototypeHost) policyForScope(scope interactiveops.Scope) (interactiveops.Policy, error) {
	surface, _, err := h.catalogs.session(h.service, scope.ActorID, scope.SessionID)
	if err != nil {
		return interactiveops.Policy{}, err
	}
	return operationsPolicy(surface), nil
}

func (h *prototypeHost) configDirForScope(scope interactiveops.Scope) (string, error) {
	_, configDir, err := h.catalogs.session(h.service, scope.ActorID, scope.SessionID)
	return configDir, err
}

func (h *prototypeHost) runRepositoryForScope(ctx context.Context, scope interactiveops.Scope, runKey string) (string, error) {
	executor, ok := h.pool.byOwner[scope.ActorID]
	if !ok {
		return "", fmt.Errorf("unknown local actor")
	}
	var snapshot string
	err := h.service.WithIdleSnapshot(scope.ActorID, scope.SessionID, func() error {
		var captureErr error
		snapshot, captureErr = executor.CaptureRunSnapshot(ctx, scope.ActorID, scope.SessionID, runKey)
		return captureErr
	})
	return snapshot, err
}

func (h *prototypeHost) repositoryForScope(scope interactiveops.Scope) (string, error) {
	if h.service == nil {
		return "", fmt.Errorf("session service unavailable")
	}
	if _, err := h.service.Session(scope.ActorID, scope.SessionID); err != nil {
		return "", err
	}
	executor, ok := h.pool.byOwner[scope.ActorID]
	if !ok {
		return "", fmt.Errorf("unknown local actor")
	}
	return executor.PrepareWorkspace(context.Background(), scope.ActorID, scope.SessionID)
}

func (h *prototypeHost) resolveAuthority(ctx context.Context, req interactive.ExecutionRequest, provider auth.Provider) (auth.ConnectionRef, error) {
	_, bindings, err := h.catalogs.execution(req)
	if err != nil {
		return auth.ConnectionRef{}, err
	}
	if err := h.service.CheckExecution(req.Owner, req.SessionID, req.TurnID, req.LeaseID, req.Fence); err != nil {
		return auth.ConnectionRef{}, err
	}
	configured := false
	for _, name := range req.AuthorityRefs {
		binding, ok := bindings[name]
		if !ok {
			return auth.ConnectionRef{}, fmt.Errorf("authority reference is not configured")
		}
		if binding.Provider == "ado" && provider == auth.ADO || binding.Provider == "app-insights" && provider == auth.AppInsights {
			configured = true
		}
	}
	if !configured {
		return auth.ConnectionRef{}, nil
	}
	connections, err := h.broker.ListConnections(h.principals[req.Owner])
	if err != nil {
		return auth.ConnectionRef{}, err
	}
	for _, connection := range connections {
		if connection.Provider == provider && connection.Mode == auth.ControlledLocal {
			return connection.Ref, nil
		}
	}
	return auth.ConnectionRef{}, fmt.Errorf("configured controlled provider is unavailable")
}

func (h *prototypeHost) agentTools(ctx context.Context, req interactive.ExecutionRequest, workspace string, emit func(string, map[string]any) error) (interactiveagent.Options, error) {
	if _, _, err := h.catalogs.execution(req); err != nil {
		return interactiveagent.Options{}, err
	}
	return (agentToolHost{sessions: h.service, ops: h.operations, executor: h.pool.byOwner[req.Owner], catalogs: h.catalogs}).options(req, emit), nil
}

func (h *prototypeHost) reviewChild(ctx context.Context, req interactive.ExecutionRequest, workspace string, emit func(string, map[string]any) error) (string, error) {
	surface, _, err := h.catalogs.execution(req)
	if err != nil {
		return "", err
	}
	if len(req.ChildCommandCapabilities) == 0 {
		return "", fmt.Errorf("DSL does not authorize child workflow commands")
	}
	h.catalogs.mu.RLock()
	configDir := h.catalogs.current.ConfigDir
	h.catalogs.mu.RUnlock()
	policy := operationsPolicy(surface)
	policy.AllowAuthoring = true // Exact human-approved child only; no child model tools.
	policy.Workflows = []string{}
	childOps, err := interactiveops.New(interactiveops.Options{Root: filepath.Join(h.config.stateDir, "helper"), Binary: h.config.binary, Repository: workspace, ConfigDir: configDir, RunTimeout: time.Minute, MaxActive: 4, MaxActivePerOwner: 2,
		PolicyForScope: func(interactiveops.Scope) (interactiveops.Policy, error) {
			if _, _, err := h.catalogs.execution(req); err != nil {
				return interactiveops.Policy{}, err
			}
			return policy, nil
		},
		RepositoryForScope: func(interactiveops.Scope) (string, error) { return workspace, nil }})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(req.WorkflowYAML))
	digest := ""
	if req.WorkflowYAML != "" {
		digest = hex.EncodeToString(sum[:])
	}
	action, err := childOps.RunReviewChildWithOptions(ctx, interactiveops.Scope{ActorID: req.Owner, SessionID: req.SessionID + "--" + req.ChildID}, req.ChildID, req.Text, interactiveops.ReviewChildOptions{WorkflowYAML: req.WorkflowYAML, ApprovedPayloadDigest: digest}, func(event interactiveops.ReviewChildEvent) error {
		data := map[string]any{"name": "child.native-workflow", "executionMode": "native-goobers", "stage": event.Kind, "childId": req.ChildID, "workflowYaml": event.WorkflowYAML, "result": event.Action}
		if event.Action != nil {
			data["output"], data["status"], data["runId"] = event.Action.Output, event.Action.Status, event.Action.RunID
		}
		if event.WorkflowYAML != "" {
			data["output"] = event.WorkflowYAML
		}
		return emit("tool.completed", data)
	})
	if err != nil {
		if errors.Is(err, interactiveops.ErrChildCustodyPending) || (action.Kind == "submit_run" && !action.CleanupConfirmed && action.OperationStatus != "prepared" && action.OperationStatus != "failed") {
			err = errors.Join(err, invoke.ErrWorkspaceNotQuiescent)
		}
		return "", err
	}
	return fmt.Sprintf("Native test-only child workflow %s: %s. Run %s; process cleanup confirmed. The independent snapshot preserves the parent unchanged. This is test evidence, not model-driven semantic review.\n%s", action.Request.Workflow, action.Status, action.RunID, action.Output), nil
}

func (h *prototypeHost) helper(ctx context.Context, req interactive.ExecutionRequest, emit func(interactive.ExecutionEvent) error) (string, error) {
	if _, _, err := h.catalogs.execution(req); err != nil {
		return "", err
	}
	if !slices.Contains(req.Tools, "session_context") {
		return "", nil
	}
	scope := interactiveops.Scope{ActorID: req.Owner, SessionID: req.SessionID}
	lower := strings.ToLower(req.Text)
	for _, intent := range []string{"design workflow", "add workflow", "create workflow", "disable workflow", "enable workflow", "run workflow", "submit run", "rename gaggle", "disable gaggle", "enable gaggle"} {
		if strings.Contains(lower, intent) {
			if !slices.Contains(req.Tools, "propose_action") {
				return "", fmt.Errorf("DSL does not allow management proposals")
			}
			action, err := h.operations.Chat(ctx, scope, req.Text, req.TurnID)
			if err != nil {
				return "", err
			}
			if err := emit(interactive.ExecutionEvent{Type: "tool.completed", Data: map[string]any{"name": "helper.proposal", "actionId": action.ID, "result": action, "status": "proposed"}}); err != nil {
				return "", err
			}
			return "Prepared a validated proposal: " + action.Summary + ". Review the exact payload in Actions and apply it to this isolated local instance. No change has been applied yet.", nil
		}
	}
	if strings.Contains(lower, "gaggle") || strings.Contains(lower, "workflow") || strings.Contains(lower, "status") || strings.Contains(lower, "backlog") {
		state, err := h.operations.Snapshot(ctx, scope)
		if err != nil {
			return "", err
		}
		data, _ := json.MarshalIndent(state, "", "  ")
		return "Observed isolated Goobers instance state (configuration and run journals are real; backlog is a controlled ADO-shaped fixture):\n" + string(data), nil
	}
	return "", nil
}

func (h *prototypeHost) stopSession(ctx context.Context, owner, sessionID string) error {
	if err := h.pool.byOwner[owner].Stop(ctx, owner, sessionID); err != nil {
		return err
	}
	return h.operations.StopSession(ctx, interactiveops.Scope{ActorID: owner, SessionID: sessionID})
}

func (h *prototypeHost) operationScope(ctx context.Context, sessionID string) (interactiveops.Scope, error) {
	owner, _ := ctx.Value(actorKey{}).(string)
	_, err := h.service.Session(owner, sessionID)
	if err != nil {
		return interactiveops.Scope{}, err
	}
	if method, _ := ctx.Value(methodKey{}).(string); method != "GET" {
		if err := h.service.CheckControl(owner, sessionID); err != nil {
			return interactiveops.Scope{}, err
		}
	}
	return interactiveops.Scope{ActorID: owner, SessionID: sessionID}, nil
}

func (h *prototypeHost) operationsHandler(operationsHTTP http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		owner, _ := h.human.authorize(r)
		sessionID := r.URL.Query().Get("sessionId")
		if _, err := h.service.Session(owner, sessionID); err != nil {
			fail(w, 403, err)
			return
		}
		surface, _, err := h.catalogs.session(h.service, owner, sessionID)
		if err != nil {
			// Revocation prevents new effects, not inspection of already-owned
			// receipts. This path cannot initialize or reconcile a native scope.
			if r.Method == "GET" {
				if strings.HasSuffix(r.URL.Path, "/state") || r.URL.Path == api+"/operations/" {
					state, readErr := h.operations.SnapshotRecorded(r.Context(), interactiveops.Scope{ActorID: owner, SessionID: sessionID})
					if readErr != nil {
						fail(w, 404, readErr)
						return
					}
					writeJSON(w, state)
					return
				}
				if strings.Contains(r.URL.Path, "/actions/") {
					operationsHTTP.ServeHTTP(w, r)
					return
				}
			}
			fail(w, 403, err)
			return
		}
		required := "propose_action"
		if r.Method == "GET" {
			required = "session_context"
		}
		if strings.Contains(r.URL.Path, "/actions/") && r.Method == "GET" {
			required = "inspect_action"
		}
		if strings.HasSuffix(r.URL.Path, "/validate-workflow") {
			required = "validate_workflow"
		}
		if !slices.Contains(surface.Tools, required) {
			fail(w, 403, fmt.Errorf("operation is not allowed by the surface DSL tool policy"))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/chat") {
			session, err := h.service.Session(owner, sessionID)
			if err != nil || session.ExecutionMode != interactive.ExecutionModeLocalTools {
				fail(w, 403, fmt.Errorf("deterministic chat routing is available only in explicit demo sessions"))
				return
			}
		}
		operationsHTTP.ServeHTTP(w, r)
	}
}

func (h *prototypeHost) reloadConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	revision, err := h.catalogs.reload(h.config.configDir, h.config.stateDir, h.service)
	if err != nil {
		fail(w, 409, err)
		return
	}
	writeJSON(w, map[string]string{"status": "reloaded", "configRevision": revision})
}

func (h *prototypeHost) status(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	owner, _ := h.human.authorize(r)
	connections, err := h.broker.ListConnections(h.principals[owner])
	if err != nil {
		fail(w, 503, err)
		return
	}
	items := []any{}
	for _, c := range connections {
		resources := []string{}
		for _, resource := range c.Resources {
			data, _ := json.Marshal(resource)
			resources = append(resources, string(data))
		}
		items = append(items, map[string]any{"id": c.Ref.ID, "provider": c.Provider, "label": c.Label, "status": c.State, "resources": resources, "expiresAt": c.ExpiresAt, "refreshCount": c.RefreshCount})
	}
	writeJSON(w, map[string]any{"providerMode": "controlled-local", "executionMode": "per-session", "defaultMode": "codex-local", "executionModes": []any{map[string]any{"id": "codex-local", "label": "Agent", "available": h.config.localLogin, "liveQualified": false, "reason": "Requires explicit trusted local model login opt-in; live model qualification is pending"}, map[string]any{"id": "local-tools", "label": "Demo tools", "available": true, "liveQualified": false}}, "identity": map[string]string{"name": owner, "subject": owner, "issuer": "goobers:controlled-local"}, "localActors": h.actors, "connections": items, "limits": map[string]any{"maxTurnSeconds": 600, "maxChildren": 3, "maxActive": 4, "leaseSeconds": 30}, "capabilities": map[string]bool{"liveModel": false, "modelEnabled": h.config.localLogin, "liveDelegatedAuth": false}})
}

func (h *prototypeHost) selectIdentity(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&request); err != nil {
		fail(w, 400, err)
		return
	}
	if err := h.human.selectActor(w, r, request.Name); err != nil {
		fail(w, 403, err)
		return
	}
	writeJSON(w, map[string]string{"actor": request.Name, "mode": "controlled-local"})
}

func (h *prototypeHost) connections(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	owner, _ := h.human.authorize(r)
	p := h.principals[owner]
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, api+"/connections/"), "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	connections, err := h.broker.ListConnections(p)
	if err != nil {
		fail(w, 503, err)
		return
	}
	var connection *auth.Connection
	for i := range connections {
		if connections[i].Ref.ID == parts[0] {
			connection = &connections[i]
		}
	}
	if connection == nil {
		http.NotFound(w, r)
		return
	}
	switch parts[1] {
	case "expire":
		err = h.broker.SetDemoAuthState(r.Context(), p, connection.Ref, "interaction_required")
	case "refresh":
		err = h.broker.SetDemoAuthState(r.Context(), p, connection.Ref, "expired")
	case "reauthenticate":
		_, err = h.broker.Reauthenticate(r.Context(), p, connection.Ref)
	case "revoke":
		err = h.broker.RevokeConnection(r.Context(), p, connection.Ref)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		fail(w, 409, err)
		return
	}
	response := map[string]any{"status": "confirmed", "mode": "controlled-local"}
	if parts[1] == "revoke" {
		response["sessionStops"] = revokeOwned(r.Context(), h.service, h.broker, p, connection.Ref.ID)
	}
	writeJSON(w, response)
}
