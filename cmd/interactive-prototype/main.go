// interactive-prototype is deliberately separate from the production daemon.
// It binds loopback only and uses explicitly controlled local identities and
// provider fixtures. No Microsoft credential acquisition or live grants exist.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/goobers/goobers/internal/interactive"
	"github.com/goobers/goobers/internal/interactiveagent"
	auth "github.com/goobers/goobers/internal/interactiveauth"
	"github.com/goobers/goobers/internal/interactiveexec"
	"github.com/goobers/goobers/internal/interactiveops"
	"github.com/goobers/goobers/internal/invoke"
)

const api = "/api/v1/interactive"

type actorKey struct{}
type methodKey struct{}
type executors struct {
	byOwner map[string]*interactiveexec.Executor
}

func (e *executors) Execute(ctx context.Context, r interactive.ExecutionRequest, emit func(interactive.ExecutionEvent) error) (interactive.ExecutionResult, error) {
	x, ok := e.byOwner[r.Owner]
	if !ok {
		return interactive.ExecutionResult{}, fmt.Errorf("unknown configured actor")
	}
	return x.Execute(ctx, r, emit)
}
func (e *executors) Recover(ctx context.Context, r interactive.ExecutionRequest) error {
	x, ok := e.byOwner[r.Owner]
	if !ok {
		return fmt.Errorf("unknown configured actor")
	}
	return x.Recover(ctx, r)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "agent-tools" {
		flags := flag.NewFlagSet("agent-tools", flag.ContinueOnError)
		socket := flags.String("socket", "", "private invocation socket")
		if err := flags.Parse(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		if *socket == "" || flags.NArg() != 0 {
			log.Fatal("agent-tools requires only --socket")
		}
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer cancel()
		if err := interactiveagent.ServeStdio(ctx, *socket, os.Stdin, os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := serve(); err != nil {
		log.Fatal(err)
	}
}

func serve() (serveErr error) {
	listen := flag.String("listen", "127.0.0.1:8097", "loopback HTTP address")
	stateDir := flag.String("state-dir", "", "private durable state directory outside the source repository (required)")
	source := flag.String("source", "samples/interactive-investigation", "trusted local repository snapshot source")
	assets := flag.String("assets", "internal/portalassets/dist", "built portal directory")
	binary := flag.String("goobers-bin", ".tools/goobers", "native Goobers CLI for Helper workflow operations")
	localLogin := flag.Bool("trusted-local-model-login", false, "explicitly use existing trusted local Codex login; never provider delegation")
	actorsFlag := flag.String("local-actors", "Mason,Alex", "controlled local personas for isolation demonstrations; not real remote authentication")
	configDir := flag.String("config-dir", "samples/interactive-config", "normal Goobers config tree declaring DSL 3.0 interactive surfaces")
	flag.Parse()
	if *stateDir == "" {
		return fmt.Errorf("--state-dir is required; use a private directory outside git")
	}
	address, port, err := net.SplitHostPort(*listen)
	if err != nil {
		return err
	}
	ip := net.ParseIP(address)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("prototype requires a literal loopback --listen address")
	}
	*stateDir, err = filepath.Abs(*stateDir)
	if err != nil {
		return err
	}
	*source, err = filepath.Abs(*source)
	if err != nil {
		return err
	}
	*assets, err = filepath.Abs(*assets)
	if err != nil {
		return err
	}
	*binary, err = filepath.Abs(*binary)
	if err != nil {
		return err
	}
	if relative, err := filepath.Rel(*source, *stateDir); err == nil && (relative == "." || !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && relative != "..") {
		return fmt.Errorf("state directory must not be inside snapshot source")
	}
	if err := os.MkdirAll(*stateDir, 0700); err != nil {
		return err
	}
	if err := os.Chmod(*stateDir, 0700); err != nil {
		return err
	}
	for path := *stateDir; ; path = filepath.Dir(path) {
		if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
			return fmt.Errorf("--state-dir must be outside any git checkout")
		}
		if filepath.Dir(path) == path {
			break
		}
	}
	catalog, err := loadDSLCatalog(*configDir, *stateDir)
	if err != nil {
		return err
	}
	catalogs := &catalogStore{current: catalog}
	selfBinary, err := os.Executable()
	if err != nil {
		return err
	}
	availableModes := []string{interactive.ExecutionModeLocalTools}
	if *localLogin {
		availableModes = append(availableModes, interactive.ExecutionModeCodexLocal)
	}
	actors := strings.Split(*actorsFlag, ",")
	for i := range actors {
		actors[i] = strings.TrimSpace(actors[i])
		if actors[i] == "" || len(actors[i]) > 80 {
			return fmt.Errorf("local actor names must be nonempty and at most 80 characters")
		}
	}
	actors = slices.Compact(actors)
	var service *interactive.Service
	broker, err := auth.Open(auth.Config{Path: filepath.Join(*stateDir, "broker.json"), CheckExecution: func(ctx context.Context, identity auth.Identity, request auth.OperationRequest) error {
		if service == nil {
			return fmt.Errorf("session authority unavailable")
		}
		return service.CheckExecution(identity.Subject, request.SessionID, request.TurnID, request.LeaseID, request.ExecutionFence)
	}})
	if err != nil {
		return err
	}
	defer func() { serveErr = errors.Join(serveErr, broker.Close()) }()
	principals := map[string]auth.Principal{}
	for _, actor := range actors {
		p, err := auth.LocalPrincipal(actor)
		if err != nil {
			return err
		}
		principals[actor] = p
		for _, provider := range []auth.Provider{auth.ADO, auth.AppInsights} {
			if _, err := broker.ConnectDemo(context.Background(), p, provider); err != nil {
				return err
			}
		}
	}
	pool := &executors{byOwner: map[string]*interactiveexec.Executor{}}
	operations, err := interactiveops.New(interactiveops.Options{Root: filepath.Join(*stateDir, "helper"), Binary: *binary, Repository: *source, RunTimeout: time.Minute, MaxActive: 4, MaxActivePerOwner: 2,
		PolicyForScope: func(scope interactiveops.Scope) (interactiveops.Policy, error) {
			surface, _, err := catalogs.session(service, scope.ActorID, scope.SessionID)
			if err != nil {
				return interactiveops.Policy{}, err
			}
			return operationsPolicy(surface), nil
		},
		ConfigDirForScope: func(scope interactiveops.Scope) (string, error) {
			_, configDir, err := catalogs.session(service, scope.ActorID, scope.SessionID)
			return configDir, err
		},
		RunRepositoryForScope: func(ctx context.Context, scope interactiveops.Scope, runKey string) (string, error) {
			executor, ok := pool.byOwner[scope.ActorID]
			if !ok {
				return "", fmt.Errorf("unknown local actor")
			}
			var snapshot string
			err := service.WithIdleSnapshot(scope.ActorID, scope.SessionID, func() error {
				var captureErr error
				snapshot, captureErr = executor.CaptureRunSnapshot(ctx, scope.ActorID, scope.SessionID, runKey)
				return captureErr
			})
			return snapshot, err
		},
		RepositoryForScope: func(scope interactiveops.Scope) (string, error) {
			if service == nil {
				return "", fmt.Errorf("session service unavailable")
			}
			if _, err := service.Session(scope.ActorID, scope.SessionID); err != nil {
				return "", err
			}
			executor, ok := pool.byOwner[scope.ActorID]
			if !ok {
				return "", fmt.Errorf("unknown local actor")
			}
			return executor.PrepareWorkspace(context.Background(), scope.ActorID, scope.SessionID)
		}})
	if err != nil {
		return err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		serveErr = errors.Join(serveErr, operations.Close(ctx))
	}()
	for _, actor := range actors {
		executor, err := interactiveexec.New(interactiveexec.Config{StateDir: *stateDir, SourceDir: *source, Mode: "local-tools", SelfBinary: selfBinary, Broker: broker, Principal: principals[actor], TrustedLocalModelLogin: *localLogin, HeartbeatInterval: 250 * time.Millisecond,
			ResolveAuthority: func(ctx context.Context, req interactive.ExecutionRequest, provider auth.Provider) (auth.ConnectionRef, error) {
				_, bindings, err := catalogs.execution(req)
				if err != nil {
					return auth.ConnectionRef{}, err
				}
				if err := service.CheckExecution(req.Owner, req.SessionID, req.TurnID, req.LeaseID, req.Fence); err != nil {
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
				connections, err := broker.ListConnections(principals[req.Owner])
				if err != nil {
					return auth.ConnectionRef{}, err
				}
				for _, connection := range connections {
					if connection.Provider == provider && connection.Mode == auth.ControlledLocal {
						return connection.Ref, nil
					}
				}
				return auth.ConnectionRef{}, fmt.Errorf("configured controlled provider is unavailable")
			},
			AgentTools: func(ctx context.Context, req interactive.ExecutionRequest, workspace string, emit func(string, map[string]any) error) (interactiveagent.Options, error) {
				if _, _, err := catalogs.execution(req); err != nil {
					return interactiveagent.Options{}, err
				}
				return (agentToolHost{sessions: service, ops: operations, executor: pool.byOwner[req.Owner], catalogs: catalogs}).options(req, emit), nil
			},
			ReviewChild: func(ctx context.Context, req interactive.ExecutionRequest, workspace string, emit func(string, map[string]any) error) (string, error) {
				surface, _, err := catalogs.execution(req)
				if err != nil {
					return "", err
				}
				if len(req.ChildCommandCapabilities) == 0 {
					return "", fmt.Errorf("DSL does not authorize child workflow commands")
				}
				catalogs.mu.RLock()
				configDir := catalogs.current.ConfigDir
				catalogs.mu.RUnlock()
				policy := operationsPolicy(surface)
				policy.AllowAuthoring = true // Exact human-approved child only; no child model tools.
				policy.Workflows = []string{}
				childOps, err := interactiveops.New(interactiveops.Options{Root: filepath.Join(*stateDir, "helper"), Binary: *binary, Repository: workspace, ConfigDir: configDir, RunTimeout: time.Minute, MaxActive: 4, MaxActivePerOwner: 2,
					PolicyForScope: func(interactiveops.Scope) (interactiveops.Policy, error) {
						if _, _, err := catalogs.execution(req); err != nil {
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
			},
			Helper: func(ctx context.Context, req interactive.ExecutionRequest, emit func(interactive.ExecutionEvent) error) (string, error) {
				if _, _, err := catalogs.execution(req); err != nil {
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
						action, err := operations.Chat(ctx, scope, req.Text, req.TurnID)
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
					state, err := operations.Snapshot(ctx, scope)
					if err != nil {
						return "", err
					}
					data, _ := json.MarshalIndent(state, "", "  ")
					return "Observed isolated Goobers instance state (configuration and run journals are real; backlog is a controlled ADO-shaped fixture):\n" + string(data), nil
				}
				return "", nil
			},
		})
		if err != nil {
			return err
		}
		pool.byOwner[actor] = executor
	}
	principal := func(r *http.Request) (string, error) {
		owner, ok := r.Context().Value(actorKey{}).(string)
		if !ok {
			return "", fmt.Errorf("local actor unavailable")
		}
		return owner, nil
	}
	service, err = interactive.Open(interactive.Options{StateDir: filepath.Join(*stateDir, "sessions"), Surfaces: catalog.Surfaces, AvailableExecutionModes: availableModes, DefaultExecutionMode: "codex-local", Executor: pool, Principal: principal, WorkerID: "local-worker", LeaseDuration: 30 * time.Second, MaxActive: 4, MaxActivePerOwner: 2, MaxTurnDuration: 10 * time.Minute,
		OnStop: func(ctx context.Context, owner, sessionID string) error {
			if err := pool.byOwner[owner].Stop(ctx, owner, sessionID); err != nil {
				return err
			}
			return operations.StopSession(ctx, interactiveops.Scope{ActorID: owner, SessionID: sessionID})
		},
	})
	if err != nil {
		return err
	}
	defer func() { serveErr = errors.Join(serveErr, service.Close()) }()
	mux := http.NewServeMux()
	operationsHTTP := operations.Handler(func(ctx context.Context, sessionID string) (interactiveops.Scope, error) {
		owner, _ := ctx.Value(actorKey{}).(string)
		_, err := service.Session(owner, sessionID)
		if err != nil {
			return interactiveops.Scope{}, err
		}
		if method, _ := ctx.Value(methodKey{}).(string); method != "GET" {
			if err := service.CheckControl(owner, sessionID); err != nil {
				return interactiveops.Scope{}, err
			}
		}
		return interactiveops.Scope{ActorID: owner, SessionID: sessionID}, nil
	})
	mux.HandleFunc(api+"/operations/", func(w http.ResponseWriter, r *http.Request) {
		owner, _ := principal(r)
		sessionID := r.URL.Query().Get("sessionId")
		if _, err := service.Session(owner, sessionID); err != nil {
			fail(w, 403, err)
			return
		}
		surface, _, err := catalogs.session(service, owner, sessionID)
		if err != nil {
			// Revocation prevents new effects, not inspection of already-owned
			// receipts. This path cannot initialize or reconcile a native scope.
			if r.Method == "GET" {
				if strings.HasSuffix(r.URL.Path, "/state") || r.URL.Path == api+"/operations/" {
					state, readErr := operations.SnapshotRecorded(r.Context(), interactiveops.Scope{ActorID: owner, SessionID: sessionID})
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
			session, err := service.Session(owner, sessionID)
			if err != nil || session.ExecutionMode != interactive.ExecutionModeLocalTools {
				fail(w, 403, fmt.Errorf("deterministic chat routing is available only in explicit demo sessions"))
				return
			}
		}
		operationsHTTP.ServeHTTP(w, r)
	})
	mux.HandleFunc(api+"/config/reload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "method not allowed", 405)
			return
		}
		revision, err := catalogs.reload(*configDir, *stateDir, service)
		if err != nil {
			fail(w, 409, err)
			return
		}
		writeJSON(w, map[string]string{"status": "reloaded", "configRevision": revision})
	})
	mux.HandleFunc(api+"/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "method not allowed", 405)
			return
		}
		owner, _ := principal(r)
		connections, err := broker.ListConnections(principals[owner])
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
		writeJSON(w, map[string]any{"providerMode": "controlled-local", "executionMode": "per-session", "defaultMode": "codex-local", "executionModes": []any{map[string]any{"id": "codex-local", "label": "Agent", "available": *localLogin, "liveQualified": false, "reason": "Requires explicit trusted local model login opt-in; live model qualification is pending"}, map[string]any{"id": "local-tools", "label": "Demo tools", "available": true, "liveQualified": false}}, "identity": map[string]string{"name": owner, "subject": owner, "issuer": "goobers:controlled-local"}, "localActors": actors, "connections": items, "limits": map[string]any{"maxTurnSeconds": 600, "maxChildren": 3, "maxActive": 4, "leaseSeconds": 30}, "capabilities": map[string]bool{"liveModel": false, "modelEnabled": *localLogin, "liveDelegatedAuth": false}})
	})
	mux.HandleFunc(api+"/identity/select", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "method not allowed", 405)
			return
		}
		var request struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&request); err != nil {
			fail(w, 400, err)
			return
		}
		if !slices.Contains(actors, request.Name) {
			fail(w, 403, fmt.Errorf("unknown controlled local actor"))
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "goobers-local-actor", Value: url.QueryEscape(request.Name), Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		writeJSON(w, map[string]string{"actor": request.Name, "mode": "controlled-local"})
	})
	mux.HandleFunc(api+"/connections/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "method not allowed", 405)
			return
		}
		owner, _ := principal(r)
		p := principals[owner]
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, api+"/connections/"), "/")
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		connections, err := broker.ListConnections(p)
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
			err = broker.SetDemoAuthState(r.Context(), p, connection.Ref, "interaction_required")
		case "refresh":
			err = broker.SetDemoAuthState(r.Context(), p, connection.Ref, "expired")
		case "reauthenticate":
			_, err = broker.Reauthenticate(r.Context(), p, connection.Ref)
		case "revoke":
			err = broker.RevokeConnection(r.Context(), p, connection.Ref)
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
			response["sessionStops"] = revokeOwned(r.Context(), service, broker, p, connection.Ref.ID)
		}
		writeJSON(w, response)
	})
	mux.Handle(api+"/", service.Handler())
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		fail(w, 404, fmt.Errorf("this experimental server exposes interactive APIs; ordinary daemon endpoints remain on the ordinary daemon"))
	})
	mux.Handle("/", http.FileServer(http.Dir(*assets)))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, requestPort, err := net.SplitHostPort(r.Host)
		if err != nil || requestPort != port || !(host == address || host == "localhost") {
			http.Error(w, "unexpected host", 403)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host {
			http.Error(w, "cross-origin request refused", 403)
			return
		}
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			http.Error(w, "cross-site request refused", 403)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" && strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			http.Error(w, "application/json is required", 415)
			return
		}
		owner := actors[0]
		if cookie, err := r.Cookie("goobers-local-actor"); err == nil {
			decoded, err := url.QueryUnescape(cookie.Value)
			if err == nil && slices.Contains(actors, decoded) {
				owner = decoded
			}
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		ctx := context.WithValue(r.Context(), actorKey{}, owner)
		ctx = context.WithValue(ctx, methodKey{}, r.Method)
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
	server := &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	stopped := make(chan struct{})
	shutdownResult := make(chan error, 1)
	go func() {
		select {
		case <-stop:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			shutdownResult <- errors.Join(service.Close(), operations.Close(ctx), server.Shutdown(ctx))
		case <-stopped:
		}
	}()
	log.Printf("Experimental interactive prototype: http://%s/#/interactive (DSL=%s; per-session adapters; providers=controlled-local; identities=controlled-local)", *listen, catalog.Revision)
	err = server.ListenAndServe()
	close(stopped)
	if errors.Is(err, http.ErrServerClosed) {
		return <-shutdownResult
	}
	return err
}

// The connection fence is already durable before this cancellation work starts.
// Report each session's actual observed state rather than claiming process stop.
func revokeOwned(ctx context.Context, s *interactive.Service, b *auth.Broker, p auth.Principal, connectionID string) []map[string]string {
	result := []map[string]string{}
	authorities, err := b.ListAuthorities(p)
	if err != nil {
		return []map[string]string{{"status": "pending", "reason": err.Error()}}
	}
	affected := map[string]bool{}
	for _, a := range authorities {
		for _, binding := range a.Authority.Bindings {
			if binding.Connection.ID == connectionID {
				affected[a.Authority.SessionID] = true
			}
		}
	}
	for sessionID := range affected {
		status := "pending"
		for attempt := 0; attempt < 3; attempt++ {
			session, err := s.Session(p.Identity().Subject, sessionID)
			if err != nil {
				break
			}
			if session.Status == "stopped" {
				status = "stopped"
				break
			}
			_, err = s.Command(ctx, p.Identity().Subject, sessionID, interactive.Command{CommandID: fmt.Sprintf("revoke-%s-%d", connectionID, session.Sequence), Type: "stop", ExpectedSequence: session.Sequence, ExpectedFence: session.Fence})
			if err == nil {
				status = "requested"
				break
			}
			var apiError *interactive.APIError
			if !errors.As(err, &apiError) || apiError.Code != "stale_sequence" {
				break
			}
		}
		result = append(result, map[string]string{"sessionId": sessionID, "status": status})
	}
	return result
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "prototype_error", "message": err.Error()}})
}
