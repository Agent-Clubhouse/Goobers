// interactive-prototype is deliberately separate from the production daemon.
// It binds loopback only and uses explicitly controlled local identities and
// provider fixtures. No Microsoft credential acquisition or live grants exist.
package main

import (
	"context"
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
	mode := flag.String("executor", "local-tools", "local-tools or codex-local")
	localLogin := flag.Bool("trusted-local-model-login", false, "explicitly use existing trusted local Codex login; never provider delegation")
	actorsFlag := flag.String("local-actors", "Mason,Alex", "controlled local personas for isolation demonstrations; not real remote authentication")
	surfacesFile := flag.String("surfaces", "", "optional JSON array of customizable surfaces")
	leaseDuration := flag.Duration("lease-duration", 30*time.Second, "worker lease lifetime (minimum 3s)")
	maxActive := flag.Int("max-active", 4, "concurrent bound per execution pool: interactive turns and native Helper runs")
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
	if *leaseDuration < 3*time.Second {
		return fmt.Errorf("lease-duration must be at least 3s")
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
	operations, err := interactiveops.New(interactiveops.Options{Root: filepath.Join(*stateDir, "helper"), Binary: *binary, Repository: *source, RunTimeout: time.Minute, MaxActive: *maxActive, MaxActivePerOwner: 2, RepositoryForScope: func(scope interactiveops.Scope) (string, error) {
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
		executor, err := interactiveexec.New(interactiveexec.Config{StateDir: *stateDir, SourceDir: *source, Mode: *mode, Broker: broker, Principal: principals[actor], TrustedLocalModelLogin: *localLogin, HeartbeatInterval: *leaseDuration / 3,
			ReviewChild: func(ctx context.Context, req interactive.ExecutionRequest, workspace string, emit func(string, map[string]any) error) (string, error) {
				childOps, err := interactiveops.New(interactiveops.Options{Root: filepath.Join(*stateDir, "helper"), Binary: *binary, Repository: workspace, RunTimeout: time.Minute, MaxActive: *maxActive, MaxActivePerOwner: 2, RepositoryForScope: func(interactiveops.Scope) (string, error) { return workspace, nil }})
				if err != nil {
					return "", err
				}
				action, err := childOps.RunReviewChild(ctx, interactiveops.Scope{ActorID: req.Owner, SessionID: req.SessionID + "--" + req.ChildID}, req.ChildID, req.Text, func(event interactiveops.ReviewChildEvent) error {
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
				scope := interactiveops.Scope{ActorID: req.Owner, SessionID: req.SessionID}
				lower := strings.ToLower(req.Text)
				for _, intent := range []string{"design workflow", "add workflow", "create workflow", "disable workflow", "enable workflow", "run workflow", "submit run", "rename gaggle", "disable gaggle", "enable gaggle"} {
					if strings.Contains(lower, intent) {
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
	surfaces := []interactive.Surface{{ID: "helper", Name: "Gaggle Helper", Mode: "helper", Description: "Manage a real isolated gaggle: inspect state, author workflows, apply scoped changes and submit runs.", Enabled: true, MaxChildren: 3, Instructions: "Ground recommendations in configured workflows, runs, backlog and repository evidence. Distinguish proposals, admission, and observed results."}, {ID: "workbench", Name: "Cloud Workbench", Mode: "workbench", Description: "Direct repository investigation, editing, tests and bounded review children. This checkpoint runs on a local worker.", Enabled: true, MaxChildren: 3, Instructions: "Investigate and edit within the selected repository; preserve reviewable changes and honor steering."}}
	if *surfacesFile != "" {
		data, err := os.ReadFile(*surfacesFile)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, &surfaces); err != nil {
			return err
		}
	}
	principal := func(r *http.Request) (string, error) {
		owner, ok := r.Context().Value(actorKey{}).(string)
		if !ok {
			return "", fmt.Errorf("local actor unavailable")
		}
		return owner, nil
	}
	service, err = interactive.Open(interactive.Options{StateDir: filepath.Join(*stateDir, "sessions"), Surfaces: surfaces, Executor: pool, Principal: principal, WorkerID: "local-worker", LeaseDuration: *leaseDuration, MaxActive: *maxActive, MaxActivePerOwner: 2, MaxTurnDuration: 10 * time.Minute,
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
	mux.Handle(api+"/operations/", operations.Handler(func(ctx context.Context, sessionID string) (interactiveops.Scope, error) {
		owner, _ := ctx.Value(actorKey{}).(string)
		session, err := service.Session(owner, sessionID)
		if err != nil {
			return interactiveops.Scope{}, err
		}
		if session.Mode != "helper" {
			return interactiveops.Scope{}, fmt.Errorf("helper operations require a Helper session")
		}
		if method, _ := ctx.Value(methodKey{}).(string); method != "GET" {
			if err := service.CheckControl(owner, sessionID); err != nil {
				return interactiveops.Scope{}, err
			}
		}
		return interactiveops.Scope{ActorID: owner, SessionID: sessionID}, nil
	}))
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
		writeJSON(w, map[string]any{"providerMode": "controlled-local", "executionMode": *mode, "identity": map[string]string{"name": owner, "subject": owner, "issuer": "goobers:controlled-local"}, "localActors": actors, "connections": items, "limits": map[string]any{"maxTurnSeconds": 600, "maxChildren": 3, "maxActive": *maxActive, "leaseSeconds": leaseDuration.Seconds()}, "capabilities": map[string]bool{"liveModel": *mode == "codex-local", "liveDelegatedAuth": false}})
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
	log.Printf("Experimental interactive prototype: http://%s/#/interactive (executor=%s; providers=controlled-local; identities=controlled-local)", *listen, *mode)
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
