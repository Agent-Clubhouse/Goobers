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
	"net/http"
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
		token := flags.String("token-file", "", "private invocation authentication file")
		if err := flags.Parse(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		if *socket == "" || *token == "" || flags.NArg() != 0 {
			log.Fatal("agent-tools requires --socket and --token-file")
		}
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer cancel()
		if err := interactiveagent.ServeStdio(ctx, *socket, *token, os.Stdin, os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := serve(); err != nil {
		log.Fatal(err)
	}
}

func serve() (serveErr error) {
	config, err := parsePrototypeConfig()
	if err != nil {
		return err
	}
	catalog, err := loadDSLCatalog(config.configDir, config.stateDir)
	if err != nil {
		return err
	}
	catalogs := &catalogStore{current: catalog}
	selfBinary, err := os.Executable()
	if err != nil {
		return err
	}
	availableModes := []string{interactive.ExecutionModeLocalTools}
	if config.localLogin {
		availableModes = append(availableModes, interactive.ExecutionModeCodexLocal)
	}
	actors := strings.Split(config.actorsFlag, ",")
	for i := range actors {
		actors[i] = strings.TrimSpace(actors[i])
		if actors[i] == "" || len(actors[i]) > 80 {
			return fmt.Errorf("local actor names must be nonempty and at most 80 characters")
		}
	}
	actors = slices.Compact(actors)
	human, err := newHumanAuth(actors)
	if err != nil {
		return err
	}
	var service *interactive.Service
	host := &prototypeHost{config: config, catalogs: catalogs, human: human, actors: actors}
	broker, err := auth.Open(auth.Config{Path: filepath.Join(config.stateDir, "broker.json"), CheckExecution: host.checkExecution})
	if err != nil {
		return err
	}
	host.broker = broker
	defer func() { serveErr = errors.Join(serveErr, broker.Close()) }()
	principals := map[string]auth.Principal{}
	host.principals = principals
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
	host.pool = pool
	operations, err := interactiveops.New(interactiveops.Options{Root: filepath.Join(config.stateDir, "helper"), Binary: config.binary, Repository: config.source, RunTimeout: time.Minute, MaxActive: 4, MaxActivePerOwner: 2,
		PolicyForScope:        host.policyForScope,
		ConfigDirForScope:     host.configDirForScope,
		RunRepositoryForScope: host.runRepositoryForScope,
		RepositoryForScope:    host.repositoryForScope})
	if err != nil {
		return err
	}
	host.operations = operations
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		serveErr = errors.Join(serveErr, operations.Close(ctx))
	}()
	for _, actor := range actors {
		executor, err := interactiveexec.New(interactiveexec.Config{StateDir: config.stateDir, SourceDir: config.source, Mode: "local-tools", SelfBinary: selfBinary, Broker: broker, Principal: principals[actor], TrustedLocalModelLogin: config.localLogin, HeartbeatInterval: 250 * time.Millisecond,
			ResolveAuthority: host.resolveAuthority,
			AgentTools:       host.agentTools,
			ReviewChild:      host.reviewChild,
			Helper:           host.helper,
		})
		if err != nil {
			return err
		}
		pool.byOwner[actor] = executor
	}
	principal := human.authorize
	host.service, err = interactive.Open(interactive.Options{StateDir: filepath.Join(config.stateDir, "sessions"), Surfaces: catalog.Surfaces, AvailableExecutionModes: availableModes, DefaultExecutionMode: "codex-local", Executor: pool, Principal: principal, WorkerID: "local-worker", LeaseDuration: 30 * time.Second, MaxActive: 4, MaxActivePerOwner: 2, MaxTurnDuration: 10 * time.Minute,
		OnStop: host.stopSession,
	})
	if err != nil {
		return err
	}
	service = host.service
	defer func() { serveErr = errors.Join(serveErr, service.Close()) }()
	mux := http.NewServeMux()
	operationsHTTP := operations.Handler(host.operationScope)
	mux.HandleFunc(api+"/operations/", host.operationsHandler(operationsHTTP))
	mux.HandleFunc(api+"/config/reload", host.reloadConfig)
	mux.HandleFunc(api+"/status", host.status)
	mux.HandleFunc(api+"/identity/select", host.selectIdentity)
	mux.HandleFunc(api+"/connections/", host.connections)
	mux.Handle(api+"/", service.Handler())
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		fail(w, 404, fmt.Errorf("this experimental server exposes interactive APIs; ordinary daemon endpoints remain on the ordinary daemon"))
	})
	mux.Handle("/", http.FileServer(http.Dir(config.assets)))
	handler := authenticatedPrototypeHandler(mux, config.address, config.port, human)
	server := &http.Server{Addr: config.listen, Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute}
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
	link, err := human.writeBootstrapLink(config.stateDir, config.listen)
	if err != nil {
		return fmt.Errorf("write private human authentication link: %w", err)
	}
	defer human.removeBootstrapLink()
	log.Printf("Human-only authentication link file (open privately; do not share with agents): %s", link)
	log.Printf("Experimental interactive prototype: http://%s/#/interactive (DSL=%s; per-session adapters; providers=controlled-local; identities=controlled-local)", config.listen, catalog.Revision)
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
