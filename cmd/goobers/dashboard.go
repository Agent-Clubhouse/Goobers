package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/signals"
)

const (
	defaultDashboardPort = 8081
	dashboardModeMeta    = `<meta name="goobers-dashboard-mode"`
)

var (
	dashboardAttachTimeout = 30 * time.Second
	launchDashboardBrowser = openDashboardBrowser
)

//go:embed portal-dist
var embeddedDashboardAssets embed.FS

type dashboardMode string

const (
	dashboardModeDaemon     dashboardMode = "daemon"
	dashboardModeStandalone dashboardMode = "standalone"
)

type dashboardPort struct {
	number int
	auto   bool
}

type dashboardAPI struct {
	handler http.Handler
	mode    dashboardMode
	close   func() error
}

type standaloneDashboardReader struct {
	readservice.Reader
	identity readservice.InstanceIdentity
	loadedAt time.Time
}

func (r standaloneDashboardReader) Health(ctx context.Context) (readservice.Health, error) {
	health, err := r.Reader.Health(ctx)
	if !errors.Is(err, os.ErrNotExist) {
		return health, err
	}
	return readservice.Health{
		APIVersion:    readservice.APIVersion,
		SchemaVersion: readservice.SchemaVersion,
		Ready:         true,
		Instance:      r.identity,
		Freshness: readservice.Freshness{
			ObservedAt:          time.Now().UTC(),
			DefinitionsLoadedAt: r.loadedAt,
		},
	}, nil
}

func runDashboard(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signals.SetupSignalContext()
	defer stop()
	return runDashboardContext(ctx, args, stdout, stderr)
}

func runDashboardContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("dashboard", flag.ContinueOnError)
	flags.SetOutput(stderr)
	portValue := flags.String("port", strconv.Itoa(defaultDashboardPort), "dashboard port, or \"auto\" to use the first available port from 8081")
	noOpen := flags.Bool("no-open", false, "print the dashboard URL without opening a browser")
	devAssets := flags.String("dev-assets", "", "serve a portal build from this directory instead of embedded assets")
	flags.Usage = func() {
		pf(stderr, "Usage: goobers dashboard [--port=<port|auto>] [--no-open] [--dev-assets=<dir>] [path]\n\n"+
			"Serve the embedded portal against the live daemon when `goobers up` is\n"+
			"running, or against a standalone read-only service otherwise. The default\n"+
			"port is %d; --port=auto increments from there until a port is available.\n"+
			"Blocks until interrupted. Exit codes: 0 = clean shutdown, 1 = service or\n"+
			"browser failure, 2 = usage/IO error.\n", defaultDashboardPort)
	}
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 1 {
		flags.Usage()
		return 2
	}
	port, err := parseDashboardPort(*portValue)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	root := "."
	if flags.NArg() == 1 {
		root = flags.Arg(0)
	}
	layout := instance.NewLayout(root)
	if _, err := os.Stat(layout.ConfigFile()); err != nil {
		pf(stderr, "error: %s not found (not an instance root — run `goobers init` first)\n", layout.ConfigFile())
		return 2
	}
	config, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		pf(stderr, "error: invalid instance.yaml: %v\n", err)
		return 1
	}

	errorLog := log.New(stderr, "dashboard: ", log.LstdFlags)
	api, err := prepareDashboardAPI(ctx, layout, config, errorLog)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) && errors.Is(err, context.Canceled) {
			return 0
		}
		pf(stderr, "error: initialize dashboard API: %v\n", err)
		return 1
	}

	assets, err := dashboardAssetFS(*devAssets)
	if err != nil {
		pf(stderr, "error: load dashboard assets: %v\n", errors.Join(err, api.close()))
		return 1
	}
	handler, err := newDashboardHandler(assets, api.handler, api.mode)
	if err != nil {
		pf(stderr, "error: initialize dashboard assets: %v\n", errors.Join(err, api.close()))
		return 1
	}
	listener, err := listenDashboard(port)
	if err != nil {
		pf(stderr, "error: %v\n", errors.Join(err, api.close()))
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
		pf(stderr, "error: resolve dashboard address: %v\n", errors.Join(err, api.close()))
		return 1
	}
	dashboardURL := "http://127.0.0.1:" + portText + "/"
	pln(stdout, dashboardURL)
	if !*noOpen {
		if err := launchDashboardBrowser(ctx, dashboardURL); err != nil {
			shutdownErr := stopDashboard(server, cancelRequests, api)
			if ctx.Err() != nil {
				if shutdownErr != nil {
					pf(stderr, "error: shut down dashboard: %v\n", shutdownErr)
					return 1
				}
				return 0
			}
			pf(stderr, "error: open dashboard in browser: %v\n", errors.Join(err, shutdownErr))
			return 1
		}
	}

	select {
	case <-ctx.Done():
		err := stopDashboard(server, cancelRequests, api)
		if err != nil {
			pf(stderr, "error: shut down dashboard: %v\n", err)
			return 1
		}
		return 0
	case err := <-serveDone:
		cancelRequests()
		closeErr := api.close()
		if err == nil {
			err = errors.New("dashboard server stopped unexpectedly")
		}
		pf(stderr, "error: dashboard server stopped: %v\n", errors.Join(err, closeErr))
		return 1
	}
}

func parseDashboardPort(value string) (dashboardPort, error) {
	if value == "auto" {
		return dashboardPort{number: defaultDashboardPort, auto: true}, nil
	}
	number, err := strconv.Atoi(value)
	if err != nil || number < 1 || number > 65535 {
		return dashboardPort{}, fmt.Errorf("--port must be a number from 1 through 65535, or \"auto\"")
	}
	return dashboardPort{number: number}, nil
}

func listenDashboard(port dashboardPort) (net.Listener, error) {
	for number := port.number; number <= 65535; number++ {
		address := net.JoinHostPort("127.0.0.1", strconv.Itoa(number))
		listener, err := net.Listen("tcp", address)
		if err == nil {
			return listener, nil
		}
		if !port.auto {
			return nil, fmt.Errorf("dashboard port %d is unavailable: %w; use --port=auto to try the next available port", number, err)
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			return nil, fmt.Errorf("listen for dashboard on %s: %w", address, err)
		}
	}
	return nil, fmt.Errorf("no dashboard port is available from %d through 65535", port.number)
}

func prepareDashboardAPI(ctx context.Context, layout instance.Layout, config *instance.Config, errorLog *log.Logger) (dashboardAPI, error) {
	running, _, err := inspectDaemonLock(filepath.Join(layout.SchedulerDir(), "up.lock"))
	if err != nil {
		return dashboardAPI{}, err
	}
	if running {
		target, err := waitForDashboardDaemon(ctx, layout, config.APIListenAddress())
		if err != nil {
			return dashboardAPI{}, err
		}
		proxy := httputil.NewSingleHostReverseProxy(target)
		proxy.ErrorLog = errorLog
		return dashboardAPI{
			handler: proxy,
			mode:    dashboardModeDaemon,
			close:   func() error { return nil },
		}, nil
	}
	return standaloneDashboardAPI(layout, config, errorLog)
}

func waitForDashboardDaemon(ctx context.Context, layout instance.Layout, configuredAddress string) (*url.URL, error) {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.NewTimer(dashboardAttachTimeout)
	defer deadline.Stop()
	var lastErr error
	lastLocation := configuredAddress
	for {
		address, addressErr := dashboardDaemonAPIAddress(layout, configuredAddress)
		if addressErr != nil {
			lastErr = addressErr
		} else {
			target, parseErr := url.Parse("http://" + address)
			if parseErr != nil {
				return nil, fmt.Errorf("parse daemon API address %q: %w", address, parseErr)
			}
			lastLocation = target.String()
			request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, target.String()+httpapi.HealthPath, nil)
			if requestErr != nil {
				return nil, requestErr
			}
			response, requestErr := client.Do(request)
			if requestErr == nil {
				if response.StatusCode != http.StatusOK {
					lastErr = fmt.Errorf("health endpoint returned %s", response.Status)
				} else {
					var health readservice.Health
					switch decodeErr := json.NewDecoder(response.Body).Decode(&health); {
					case decodeErr != nil:
						lastErr = decodeErr
					case !health.Ready:
						lastErr = errors.New("daemon API is not ready")
					case health.APIVersion != readservice.APIVersion || health.SchemaVersion != readservice.SchemaVersion:
						lastErr = fmt.Errorf("daemon API contract is %s/%s, want %s/%s",
							health.APIVersion, health.SchemaVersion, readservice.APIVersion, readservice.SchemaVersion)
					default:
						lastErr = nil
					}
				}
				if closeErr := response.Body.Close(); closeErr != nil && lastErr == nil {
					lastErr = closeErr
				}
				if lastErr == nil {
					return target, nil
				}
			} else {
				lastErr = requestErr
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("live `goobers up` daemon API at %s is unavailable: %w", lastLocation, lastErr)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func dashboardDaemonAPIAddress(layout instance.Layout, configuredAddress string) (string, error) {
	data, err := os.ReadFile(filepath.Join(layout.SchedulerDir(), daemonAPIAddressFileName))
	if err == nil {
		return usableDaemonAPIAddress(strings.TrimSpace(string(data)))
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read daemon API address: %w", err)
	}
	return usableDaemonAPIAddress(configuredAddress)
}

func usableDaemonAPIAddress(address string) (string, error) {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", fmt.Errorf("invalid daemon API address %q: %w", address, err)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil {
		return "", fmt.Errorf("invalid daemon API address %q: %w", address, err)
	}
	if portNumber == 0 {
		return "", errors.New("daemon API address has not been published")
	}
	return address, nil
}

func standaloneDashboardAPI(layout instance.Layout, config *instance.Config, errorLog *log.Logger) (dashboardAPI, error) {
	definitions, report, err := loadConfigDirectory(layout.ConfigDir())
	if err != nil {
		return dashboardAPI{}, err
	}
	reads, err := readservice.NewLocal(readservice.LocalSources{
		Layout:      layout,
		Config:      config,
		Definitions: definitions,
		Validation:  report,
	}, func() bool { return true })
	if err != nil {
		return dashboardAPI{}, err
	}
	manifestInstance := definitions.Manifest.Spec.Instance
	reader := standaloneDashboardReader{
		Reader: reads,
		identity: readservice.InstanceIdentity{
			Name:        manifestInstance.Name,
			Environment: manifestInstance.Environment,
		},
		loadedAt: time.Now().UTC(),
	}
	events, err := httpapi.NewEventStream(layout, errorLog)
	if err != nil {
		return dashboardAPI{}, err
	}
	handler, err := httpapi.NewHandler(reader, httpapi.AllowAll, errorLog, httpapi.WithEventStream(events))
	if err != nil {
		events.Close()
		return dashboardAPI{}, err
	}
	return dashboardAPI{
		handler: handler,
		mode:    dashboardModeStandalone,
		close: func() error {
			events.Close()
			waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			return events.Wait(waitCtx)
		},
	}, nil
}

func dashboardAssetFS(devAssets string) (fs.FS, error) {
	if devAssets == "" {
		return fs.Sub(embeddedDashboardAssets, "portal-dist")
	}
	info, err := os.Stat(filepath.Join(devAssets, "index.html"))
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory, not an index file", filepath.Join(devAssets, "index.html"))
	}
	return os.DirFS(devAssets), nil
}

func newDashboardHandler(assets fs.FS, api http.Handler, mode dashboardMode) (http.Handler, error) {
	if assets == nil {
		return nil, errors.New("dashboard asset filesystem is required")
	}
	if api == nil {
		return nil, errors.New("dashboard API handler is required")
	}
	index, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		return nil, err
	}
	index, err = dashboardIndex(index, mode)
	if err != nil {
		return nil, err
	}
	files := http.FileServer(http.FS(assets))
	mux := http.NewServeMux()
	mux.Handle("/api/", api)
	mux.HandleFunc("/", func(response http.ResponseWriter, request *http.Request) {
		name := strings.TrimPrefix(path.Clean(request.URL.Path), "/")
		if name == "" || name == "." || name == "index.html" {
			serveDashboardIndex(response, request, index)
			return
		}
		info, err := fs.Stat(assets, name)
		if err == nil && !info.IsDir() {
			files.ServeHTTP(response, request)
			return
		}
		http.NotFound(response, request)
	})
	return mux, nil
}

func dashboardIndex(index []byte, mode dashboardMode) ([]byte, error) {
	start := bytes.Index(index, []byte(dashboardModeMeta))
	if start < 0 {
		return nil, errors.New("portal index is missing the dashboard mode marker")
	}
	endOffset := bytes.IndexByte(index[start:], '>')
	if endOffset < 0 {
		return nil, errors.New("portal dashboard mode marker is malformed")
	}
	end := start + endOffset
	tag := index[start:end]
	content := []byte(`content="daemon"`)
	if !bytes.Contains(tag, content) {
		return nil, errors.New("portal dashboard mode marker has an unsupported default")
	}
	replacement := []byte(`content="` + string(mode) + `"`)
	contentStart := start + bytes.Index(tag, content)
	result := make([]byte, 0, len(index)-len(content)+len(replacement))
	result = append(result, index[:contentStart]...)
	result = append(result, replacement...)
	result = append(result, index[contentStart+len(content):]...)
	return result, nil
}

func serveDashboardIndex(response http.ResponseWriter, request *http.Request, index []byte) {
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(response, request, "index.html", time.Time{}, bytes.NewReader(index))
}

func stopDashboard(server *http.Server, cancelRequests context.CancelFunc, api dashboardAPI) error {
	cancelRequests()
	apiErr := api.close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return errors.Join(server.Shutdown(ctx), apiErr)
}

func openDashboardBrowser(ctx context.Context, address string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.CommandContext(ctx, "open", address)
	case "windows":
		command = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", address)
	default:
		command = exec.CommandContext(ctx, "xdg-open", address)
	}
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			return errors.New("browser launcher timed out")
		case errors.Is(ctx.Err(), context.Canceled):
			return ctx.Err()
		}
		return err
	}
	return nil
}
