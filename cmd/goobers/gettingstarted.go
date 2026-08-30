package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/signals"
)

// dashboardModeGettingStarted is the portal mode the getting-started command
// stamps into the index's goobers-dashboard-mode marker. The rewrite reuses
// dashboardIndex, which validates the shipped marker exactly as `goobers
// dashboard` does, so the dashboard's own startup contract is untouched.
const dashboardModeGettingStarted dashboardMode = "getting-started"

const guidedInitBrowserHelp = "Usage: goobers init --guided [--port=<port|auto>] [--no-open] [--workdir <dir>]\n\n" +
	"Serve and open the browser-based instance setup. It inspects an existing\n" +
	"GitHub or Azure DevOps clone, discovers its identity, default branch, CI and\n" +
	"toolchain, asks only for configuration placement and desired behavior, creates\n" +
	"and validates the instance, and prepares required repository labels. It does\n" +
	"not run a workflow. Back and Continue navigation stays inside the browser,\n" +
	"while completed filesystem\n" +
	"actions remain the source of truth across restarts. Token values never\n" +
	"reach the browser or configuration files.\n\n" +
	"Configuration and instance placement are selected in the browser. --workdir\n" +
	"holds temporary browser setup state and defaults beneath the current\n" +
	"user's local application-data directory; the directory is created when\n" +
	"needed. The default --port is auto,\n" +
	"incrementing from %d until a port is available. Blocks until interrupted.\n" +
	"Exit codes: 0 = clean shutdown, 1 = service or browser failure, 2 =\n" +
	"usage/IO error.\n"

func runGuidedInitBrowser(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signals.SetupSignalContext()
	defer stop()
	return runGuidedInitBrowserContext(ctx, args, stdout, stderr)
}

func runGuidedInitBrowserContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newCLIFlagSet("init --guided", flag.ContinueOnError)
	flags.SetOutput(stderr)
	portValue := flags.String("port", "auto", "server port, or \"auto\" to use the first available port from 8081")
	noOpen := flags.Bool("no-open", false, "print the guided setup URL without opening a browser")
	workdir := flags.String("workdir", defaultGettingStartedWorkdir(), "directory holding temporary browser setup state")
	flags.Usage = func() { pf(stderr, guidedInitBrowserHelp, defaultDashboardPort) }
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return 2
	}
	port, err := parseDashboardPort(*portValue)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	absWorkdir, err := filepath.Abs(*workdir)
	if err != nil {
		pf(stderr, "error: resolve --workdir: %v\n", err)
		return 2
	}
	if err := os.MkdirAll(absWorkdir, 0o755); err != nil {
		pf(stderr, "error: create --workdir %s: %v\n", absWorkdir, err)
		return 2
	}
	absInstancePath, err := filepath.Abs(filepath.Join(filepath.Dir(absWorkdir), "instance"))
	if err != nil {
		pf(stderr, "error: resolve instance path: %v\n", err)
		return 2
	}

	errorLog := log.New(stderr, "init --guided: ", log.LstdFlags)
	guided, err := newGuidedServer(absWorkdir, absInstancePath, errorLog)
	if err != nil {
		pf(stderr, "error: initialize guided server: %v\n", err)
		return 1
	}

	assets, err := dashboardAssetFS("")
	if err != nil {
		pf(stderr, "error: load portal assets: %v\n", errors.Join(err, guided.close()))
		return 1
	}
	handler, err := newGettingStartedHandler(assets, guided)
	if err != nil {
		pf(stderr, "error: initialize portal assets: %v\n", errors.Join(err, guided.close()))
		return 1
	}
	listener, err := listenDashboard("127.0.0.1", port)
	if err != nil {
		pf(stderr, "error: %v\n", errors.Join(err, guided.close()))
		return 1
	}

	requestContext, cancelRequests := context.WithCancel(ctx)
	server := &http.Server{
		Handler:           handler,
		ErrorLog:          errorLog,
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext: func(net.Listener) context.Context {
			return requestContext
		},
	}
	serveDone := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveDone <- err
	}()

	_, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		cancelRequests()
		_ = server.Close()
		pf(stderr, "error: resolve getting-started address: %v\n", errors.Join(err, guided.close()))
		return 1
	}
	guideURL := "http://127.0.0.1:" + portText + "/#/getting-started"
	pln(stdout, guideURL)
	if !*noOpen {
		if err := launchDashboardBrowser(ctx, guideURL); err != nil {
			shutdownErr := stopGettingStarted(server, cancelRequests, guided)
			if ctx.Err() != nil {
				if shutdownErr != nil {
					pf(stderr, "error: shut down getting-started server: %v\n", shutdownErr)
					return 1
				}
				return 0
			}
			pf(stderr, "error: open getting-started guide in browser: %v\n", errors.Join(err, shutdownErr))
			return 1
		}
	}

	select {
	case <-ctx.Done():
		if err := stopGettingStarted(server, cancelRequests, guided); err != nil {
			pf(stderr, "error: shut down getting-started server: %v\n", err)
			return 1
		}
		return 0
	case err := <-serveDone:
		cancelRequests()
		closeErr := guided.close()
		if err == nil {
			err = errors.New("getting-started server stopped unexpectedly")
		}
		pf(stderr, "error: getting-started server stopped: %v\n", errors.Join(err, closeErr))
		return 1
	}
}

func stopGettingStarted(server *http.Server, cancelRequests context.CancelFunc, guided *guidedServer) error {
	cancelRequests()
	guidedErr := guided.close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return errors.Join(server.Shutdown(ctx), guidedErr)
}

func defaultGettingStartedWorkdir() string {
	cache, err := os.UserCacheDir()
	if err != nil || strings.TrimSpace(cache) == "" {
		return filepath.Join(".", ".goobers", "getting-started")
	}
	return filepath.Join(cache, "Goobers", "getting-started")
}

// newGettingStartedHandler dispatches exactly like the dashboard's manual
// dispatcher (no ServeMux — see newDashboardHandler for why), with two extra
// prefixes: /guided/ for the guided action endpoints, and a lazily constructed
// standalone read-only /api/ that appears once the tutorial instance exists.
func newGettingStartedHandler(assets fs.FS, guided *guidedServer) (http.Handler, error) {
	if assets == nil {
		return nil, errors.New("getting-started asset filesystem is required")
	}
	if guided == nil {
		return nil, errors.New("guided server is required")
	}
	index, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		return nil, err
	}
	index, err = dashboardIndex(index, dashboardModeGettingStarted)
	if err != nil {
		return nil, err
	}
	files := http.FileServer(http.FS(assets))
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/guided/") {
			guided.serveGuided(response, request)
			return
		}
		if strings.HasPrefix(request.URL.Path, "/api/") {
			guided.serveAPI(response, request)
			return
		}
		// Same co-brand override as the dashboard: an operator-supplied file in
		// the tutorial instance's assets/ dir wins; before the instance exists
		// every lookup misses and falls through to the embedded bundle.
		if strings.HasPrefix(request.URL.Path, "/assets/") &&
			serveInstanceAsset(response, request, guided.instancePath) {
			return
		}
		serveDashboardStatic(response, request, assets, files, index)
	})
	return handler, nil
}
