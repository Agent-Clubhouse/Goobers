package main

import (
	"bytes"
	"context"
	"crypto/tls"
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
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/oidcauth"
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
	launchRunDirectory     = openFilesystemPath
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
		Healthy:       true,
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

const dashboardHelp = "Usage: goobers dashboard [--port=<port|auto>] [--listen=<host:port>] [--wait-for-daemon[=<duration>]] [--no-open] [--dev-assets=<dir>] [path]\n\n" +
	"Serve the embedded portal against the live daemon when `goobers up` is\n" +
	"running, or against a standalone read-only service otherwise. The default\n" +
	"port is %d; --port=auto increments from there until a port is available.\n" +
	"--wait-for-daemon optionally waits up to 30s for a concurrently starting\n" +
	"daemon; use --wait-for-daemon=<duration> to choose another bound.\n" +
	"--listen overrides the full bind address (host:port) and takes the place\n" +
	"of --port when given; binding a non-loopback host requires api.auth to be\n" +
	"configured in instance.yaml (SEC-043) — there is no insecure override.\n" +
	"Blocks until interrupted. Exit codes: 0 = clean shutdown, 1 = service or\n" +
	"browser failure, 2 = usage/IO error.\n"

func runDashboardContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newCLIFlagSet("dashboard", flag.ContinueOnError)
	flags.SetOutput(stderr)
	portValue := flags.String("port", strconv.Itoa(defaultDashboardPort), "dashboard port, or \"auto\" to use the first available port from 8081")
	listenValue := flags.String("listen", "", "dashboard bind address as host:port, overriding --port's loopback default; "+
		"a non-loopback host requires api.auth (instance.yaml) to be configured — there is no insecure override")
	noOpen := flags.Bool("no-open", false, "print the dashboard URL without opening a browser")
	devAssets := flags.String("dev-assets", "", "serve a portal build from this directory instead of embedded assets")
	var waitForDaemon dashboardWaitFlag
	flags.Var(&waitForDaemon, "wait-for-daemon", "wait for a concurrently starting daemon (default 30s; optionally specify a duration)")
	// dashboardHelp carries a %d for the default port, so it renders here (and
	// in the registry) through defaultDashboardPort rather than via the plain
	// helpUsage path — keeping the documented port coupled to the constant.
	flags.Usage = func() { pf(stderr, dashboardHelp, defaultDashboardPort) }
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
	host := "127.0.0.1"
	if *listenValue != "" {
		host, port, err = parseDashboardListen(*listenValue)
		if err != nil {
			pf(stderr, "error: %v\n", err)
			return 2
		}
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
	if err := validateDashboardListenHost(host, config); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	errorLog := log.New(stderr, "dashboard: ", log.LstdFlags)
	api, err := prepareDashboardAPI(ctx, layout, config, errorLog, dashboardHostIsLoopback(host), waitForDaemon.duration())
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
	handler, err := newDashboardHandler(assets, api.handler, api.mode, layout.Root)
	if err != nil {
		pf(stderr, "error: initialize dashboard assets: %v\n", errors.Join(err, api.close()))
		return 1
	}
	listener, err := listenDashboard(host, port)
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
	dashboardURL := "http://" + net.JoinHostPort(host, portText) + "/"
	pln(stdout, dashboardURL)
	pf(stderr, "dashboard: mode=%s\n", api.mode)
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

type dashboardWaitFlag struct {
	enabled bool
	timeout time.Duration
}

func (f *dashboardWaitFlag) String() string {
	if !f.enabled {
		return "false"
	}
	return f.timeout.String()
}

func (f *dashboardWaitFlag) Set(value string) error {
	switch value {
	case "true":
		f.enabled = true
		f.timeout = dashboardAttachTimeout
		return nil
	case "false":
		f.enabled = false
		f.timeout = 0
		return nil
	}
	timeout, err := time.ParseDuration(value)
	if err != nil || timeout <= 0 {
		return fmt.Errorf("--wait-for-daemon must be a positive duration")
	}
	f.enabled = true
	f.timeout = timeout
	return nil
}

func (*dashboardWaitFlag) IsBoolFlag() bool { return true }

func (f dashboardWaitFlag) duration() time.Duration {
	if !f.enabled {
		return 0
	}
	return f.timeout
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

// parseDashboardListen parses --listen into a host and dashboardPort.
// Unlike --port, it does not accept "auto": a caller naming a specific
// interface is expected to know a free port there, and retry-scanning an
// arbitrary (possibly non-loopback) host is a materially different exposure
// than incrementing on loopback.
func parseDashboardListen(value string) (string, dashboardPort, error) {
	host, portText, err := net.SplitHostPort(value)
	if err != nil {
		return "", dashboardPort{}, fmt.Errorf("--listen must be a host:port address: %w", err)
	}
	if host == "" {
		return "", dashboardPort{}, fmt.Errorf("--listen host is required; wildcard listeners are not allowed")
	}
	number, err := strconv.Atoi(portText)
	if err != nil || number < 1 || number > 65535 {
		return "", dashboardPort{}, fmt.Errorf("--listen port must be a number from 1 through 65535")
	}
	return host, dashboardPort{number: number}, nil
}

// validateDashboardListenHost fails closed exactly the way instance config validation
// gates the daemon API (#640, SEC-043): a loopback host keeps the tier-1
// local-trust default, and a non-loopback host is refused unless the
// instance has an authenticator configured (api.auth.oidc) — there is
// deliberately no insecure override (#2884). Unlike the API, this does not
// also require api.tls: the dashboard's own listener never terminates TLS in
// either serving mode (daemon-attach proxies over the daemon's own
// transport; standalone speaks plain HTTP), so requiring a certificate here
// without the process ever loading or serving it would just be a config
// checkbox — transport security off-loopback is the ingress/reverse-proxy's
// job, per the documented single-HTTPS-door topology
// (deploy/reference/goobers-system/api-ingress-example.yaml).
func validateDashboardListenHost(host string, config *instance.Config) error {
	if dashboardHostIsLoopback(host) {
		return nil
	}
	if config.API.Auth == nil {
		return fmt.Errorf("--listen: host %q is not loopback: exposing the dashboard off-loopback requires "+
			"api.auth.oidc to be configured in instance.yaml so the portal is authenticated; there is no "+
			"insecure override — bind a loopback address instead (SEC-043, #2884)", host)
	}
	return nil
}

// dashboardHostIsLoopback reports whether host (a bare host, no port) is a
// loopback address or "localhost", reusing instance.IsLoopbackListenAddress
// (the same check instance config validation runs) by pairing host with a throwaway
// port purely to satisfy its host:port signature.
func dashboardHostIsLoopback(host string) bool {
	return instance.IsLoopbackListenAddress(net.JoinHostPort(host, "0"))
}

var listenDashboardTCP = net.Listen

func listenDashboard(host string, port dashboardPort) (net.Listener, error) {
	for number := port.number; number <= 65535; number++ {
		address := net.JoinHostPort(host, strconv.Itoa(number))
		listener, err := listenDashboardTCP("tcp", address)
		if err == nil {
			return listener, nil
		}
		if !port.auto {
			return nil, fmt.Errorf("dashboard listener %s is unavailable: %w; use --port=auto to try the next available port", address, err)
		}
		if !dashboardPortUnavailable(err) {
			return nil, fmt.Errorf("listen for dashboard on %s: %w", address, err)
		}
	}
	return nil, fmt.Errorf("no dashboard port is available on %s from %d through 65535", host, port.number)
}

// loopback reports whether the dashboard's own listener is bound to
// loopback, threaded down to standaloneDashboardAPI so it can gate
// WithRunRevealer the same way `goobers up` gates it on the API's own
// listen address (#2884): the reveal-in-Finder action shells out on the
// dashboard process's own machine, which is only correct when the caller is
// necessarily on that same machine — true for loopback, not guaranteed once
// --listen opts into a non-loopback bind (docs/design/portal-reveal-remote-posture.md).
func prepareDashboardAPI(ctx context.Context, layout instance.Layout, config *instance.Config, errorLog *log.Logger, loopback bool, waitForDaemon time.Duration) (dashboardAPI, error) {
	lockPath := filepath.Join(layout.SchedulerDir(), "up.lock")
	if waitForDaemon > 0 {
		if config.API.Auth != nil {
			return dashboardAPI{}, dashboardDaemonAuthError(config)
		}
		target, err := waitForDashboardDaemon(ctx, layout, daemonAPIScheme(config), config.APIListenAddress(), waitForDaemon, lockPath)
		if err != nil {
			return dashboardAPI{}, err
		}
		return dashboardDaemonAPI(target, errorLog), nil
	}
	running, _, _, err := inspectDaemonLiveness(
		lockPath,
		time.Now(),
	)
	if err != nil {
		return dashboardAPI{}, err
	}
	if running {
		// The attach proxy has no bearer-token source yet, so an authenticated
		// daemon is refused up front with the real reason instead of a probe
		// loop that times out on 401s (#640, #644).
		if config.API.Auth != nil {
			return dashboardAPI{}, dashboardDaemonAuthError(config)
		}
		target, err := waitForDashboardDaemon(ctx, layout, daemonAPIScheme(config), config.APIListenAddress(), dashboardAttachTimeout, "")
		if err != nil {
			return dashboardAPI{}, err
		}
		return dashboardDaemonAPI(target, errorLog), nil
	}
	return standaloneDashboardAPI(layout, config, errorLog, loopback)
}

func dashboardDaemonAuthError(config *instance.Config) error {
	return fmt.Errorf(
		"daemon API at %s requires a bearer token (api.auth is configured) and `goobers dashboard` cannot supply one yet; "+
			"query the daemon API directly, or stop the daemon to serve the standalone read-only dashboard",
		config.APIListenAddress(),
	)
}

func dashboardDaemonAPI(target *url.URL, errorLog *log.Logger) dashboardAPI {
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorLog = errorLog
	return dashboardAPI{
		handler: proxy,
		mode:    dashboardModeDaemon,
		close:   func() error { return nil },
	}
}

// daemonAPIScheme mirrors httpapi.Server.Scheme for the attach probe and
// proxy: the daemon serves HTTPS exactly when api.tls is configured.
func daemonAPIScheme(config *instance.Config) string {
	if config.API.TLS != nil {
		return "https"
	}
	return "http"
}

func waitForDashboardDaemon(ctx context.Context, layout instance.Layout, scheme, configuredAddress string, timeout time.Duration, lockPath string) (*url.URL, error) {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	var lastErr error
	lastLocation := scheme + "://" + configuredAddress
	for {
		lockReady := true
		if lockPath != "" {
			running, _, _, err := inspectDaemonLiveness(lockPath, time.Now())
			if err != nil {
				return nil, err
			}
			if !running {
				lastErr = fmt.Errorf("`goobers up` does not hold %s", lockPath)
				lockReady = false
			}
		}
		if lockReady {
			address, addressErr := dashboardDaemonAPIAddress(layout, configuredAddress)
			if addressErr != nil {
				lastErr = addressErr
			} else {
				target, parseErr := url.Parse(scheme + "://" + address)
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
						if lockPath != "" {
							running, _, _, lockErr := inspectDaemonLiveness(lockPath, time.Now())
							if lockErr != nil {
								return nil, lockErr
							}
							if !running {
								lastErr = fmt.Errorf("`goobers up` released %s before its API became ready", lockPath)
								lockReady = false
							}
						}
						if lockReady {
							return target, nil
						}
					}
				} else {
					// An untrusted api.tls certificate cannot heal within the
					// attach window; fail fast with the cause instead of spinning
					// to the timeout.
					var certErr *tls.CertificateVerificationError
					if errors.As(requestErr, &certErr) {
						return nil, fmt.Errorf("daemon API at %s presented a TLS certificate this host does not trust: %w; "+
							"make the api.tls certificate's issuing CA trusted on this host and retry", lastLocation, certErr)
					}
					lastErr = requestErr
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("timed out after %s waiting for live `goobers up` daemon API at %s: %w", timeout, lastLocation, lastErr)
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

func standaloneDashboardAPI(layout instance.Layout, config *instance.Config, errorLog *log.Logger, loopback bool) (dashboardAPI, error) {
	definitions, report, err := loadConfigDirectory(layout.ConfigDir())
	if err != nil {
		return dashboardAPI{}, err
	}
	// The standalone dashboard had NO projection at all (#1933, §11.2): no
	// Telemetry, no ReadModel, so every list was a full scan of all history —
	// and this is the configuration a new user meets first.
	//
	// Open the read model, building it if empty. On a read-only volume this
	// degrades EXPLICITLY rather than silently falling back to the scan.
	topology := readservice.TopologyConfig{
		Topology: readservice.TopologyStandalone,
		Layout:   layout,
	}
	readStore, readMode, _ := readservice.OpenReadModel(topology)
	if readStore != nil {
		// No measurement source here, and that is deliberate (#1782).
		//
		// The obvious move is to attach one -- the population flags come from the
		// telemetry rollup, and without a source they project as zero. But
		// standalone is contractually required to leave the instance
		// BYTE-IDENTICAL, and opening a SQLite database creates its -wal and -shm
		// alongside the file. TestStandaloneDashboardAPILeavesInstanceUnchanged
		// caught exactly that.
		//
		// Attaching is also unnecessary. Standalone constructs its service with
		// Telemetry nil, and listRunsUnannotated refuses a telemetry-backed
		// population filter with ErrTelemetryUnavailable BEFORE it dispatches to
		// the read model. So the zeroed flags are unreachable: the filter is
		// refused with a typed error rather than answered wrongly with an empty
		// page, which is the same behaviour standalone had before this change.
		if err := readservice.EnsureBuilt(context.Background(), readStore, layout, nil); err != nil {
			// A failed build degrades rather than fails: single-run routes still
			// work, and saying so beats refusing to start.
			readMode = readservice.ReadModeDegraded
		}
	}

	reads, err := readservice.NewLocal(readservice.LocalSources{
		Layout:      layout,
		Config:      config,
		Definitions: definitions,
		Validation:  report,
		ReadModel:   readStore,
	}, func() bool { return true })
	if err != nil {
		return dashboardAPI{}, err
	}
	reads.SetReadMode(readMode)
	if readStore != nil {
		reads.EnableReadModelReads()
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
	// Standalone serves live updates from the change feed too (#1929), using
	// the read model #1933 attaches. When none could be opened (a read-only
	// volume with no writable cache directory) there is no SSE, and the
	// freshness surface already renders that as degraded.
	var streamOpts []httpapi.HandlerOption
	if readStore != nil {
		streamOpts = append(streamOpts, httpapi.WithChangeFeedStream(readStore))
	}
	if loopback {
		// The reveal action shells out on this process's own machine (#2306);
		// off-loopback that machine is not necessarily the requesting user's, so
		// it is withheld rather than silently opening a window on the server —
		// the same guard `goobers up` applies to the API's own listener
		// (docs/design/portal-reveal-remote-posture.md).
		streamOpts = append(streamOpts, httpapi.WithRunRevealer(runDirectoryRevealer(layout)))
	}
	// A configured api.auth authenticates the standalone handler too, mirroring
	// how `goobers up` wires the same block into the daemon API (#640/#644).
	// This is what makes validateDashboardListenHost's off-loopback gate a
	// real enforcement rather than a config-presence formality: an operator
	// who opts a non-loopback --listen into api.auth gets a portal that
	// actually authenticates requests standalone, not just one that was
	// permitted to bind because the block happened to exist (#2884).
	standaloneAuthorizer := httpapi.AllowAll
	if auth := config.API.Auth; auth != nil && auth.OIDC != nil {
		authenticator, err := oidcauth.New(oidcauth.Config{
			Issuer:     auth.OIDC.Issuer,
			Audience:   auth.OIDC.Audience,
			RolesClaim: auth.OIDC.RolesClaimName(),
			Roles: oidcauth.RoleMapping{
				View:    auth.OIDC.Roles.View,
				Operate: auth.OIDC.Roles.Operate,
				Admin:   auth.OIDC.Roles.Admin,
			},
		})
		if err != nil {
			return dashboardAPI{}, fmt.Errorf("initialize dashboard authenticator: %w", err)
		}
		streamOpts = append(streamOpts, httpapi.WithAuthenticator(authenticator))
		standaloneAuthorizer = httpapi.RequireRoles()
	}
	handler, err := httpapi.NewHandler(reader, standaloneAuthorizer, errorLog, streamOpts...)
	if err != nil {
		return dashboardAPI{}, err
	}
	return dashboardAPI{
		handler: handler,
		mode:    dashboardModeStandalone,
		// The read model is the only thing to close now that the poller is gone
		// (#1929); the change-feed stream holds no goroutine of its own beyond
		// each subscription, which the handler cancels.
		close: func() error {
			if readStore != nil {
				return readStore.Close()
			}
			return nil
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

func newDashboardHandler(assets fs.FS, api http.Handler, mode dashboardMode, instanceRoot string) (http.Handler, error) {
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
	// A manual dispatcher rather than http.ServeMux: ServeMux redirects any
	// non-canonical path (e.g. a "/assets/../x" traversal) to its cleaned form
	// with a 3xx before dispatching, which both leaks routing behavior and
	// prevents the asset handlers' own containment checks from returning 404.
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/api/") {
			api.ServeHTTP(response, request)
			return
		}
		// Co-brand assets: an operator-supplied file in the instance's assets/
		// dir overrides the embedded bundle at the same /assets/ path; anything
		// not present there (notably the portal's own /assets/index-*.js|css)
		// falls through to the embedded file server below.
		if strings.HasPrefix(request.URL.Path, "/assets/") &&
			serveInstanceAsset(response, request, instanceRoot) {
			return
		}
		serveDashboardStatic(response, request, assets, files, index)
	})
	return handler, nil
}

// serveDashboardStatic is the shared dispatcher tail for portal-serving
// commands (`dashboard`, `getting-started`): the rewritten index for root
// paths, embedded static files when they exist, and 404 otherwise.
func serveDashboardStatic(response http.ResponseWriter, request *http.Request, assets fs.FS, files http.Handler, index []byte) {
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
}

// serveInstanceAsset serves a co-branding file from the instance's assets/ dir
// when the cleaned request path resolves to an existing regular file inside
// that dir, and reports whether it did. On any miss — traversal outside the
// dir, a directory, or a nonexistent file — it serves nothing and returns
// false so the caller falls through to the embedded bundle.
func serveInstanceAsset(w http.ResponseWriter, r *http.Request, instanceRoot string) bool {
	assetsDir := filepath.Join(instanceRoot, "assets")
	name := strings.TrimPrefix(r.URL.Path, "/assets/")
	name = filepath.FromSlash(path.Clean("/" + name))
	name = strings.TrimPrefix(name, string(filepath.Separator))
	if name == "" {
		return false
	}
	full := filepath.Join(assetsDir, name)
	rel, err := filepath.Rel(assetsDir, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		return false
	}
	http.ServeFile(w, r, full)
	return true
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
	return openNativeTarget(ctx, address, "browser launcher")
}

func openFilesystemPath(ctx context.Context, path string) error {
	return openNativeTarget(ctx, path, "file browser launcher")
}

func openNativeTarget(ctx context.Context, target, launcherName string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.CommandContext(ctx, "open", target)
	case "windows":
		command = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", target)
	default:
		command = exec.CommandContext(ctx, "xdg-open", target)
	}
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			return fmt.Errorf("%s timed out", launcherName)
		case errors.Is(ctx.Err(), context.Canceled):
			return ctx.Err()
		}
		return err
	}
	return nil
}

func runDirectoryRevealer(layout instance.Layout) func(context.Context, string) error {
	return func(ctx context.Context, runID string) error {
		dir, err := layout.FindRunDir(runID)
		if err != nil {
			return err
		}
		return launchRunDirectory(ctx, dir)
	}
}
