package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/goobers/goobers/internal/daemonstate"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readservice"
)

const dashboardTestIndex = `<!doctype html><html><head><meta name="goobers-dashboard-mode" content="daemon" /></head><body>portal</body></html>`

type dashboardURLWriter struct {
	once sync.Once
	url  chan string
}

func dashboardTestArgs(t *testing.T, args ...string) []string {
	t.Helper()
	assets := t.TempDir()
	if err := os.WriteFile(filepath.Join(assets, "index.html"), []byte(dashboardTestIndex), 0o644); err != nil {
		t.Fatal(err)
	}
	return append([]string{"--dev-assets=" + assets}, args...)
}

func (w *dashboardURLWriter) Write(data []byte) (int, error) {
	w.once.Do(func() {
		w.url <- strings.TrimSpace(string(data))
	})
	return len(data), nil
}

func TestDashboardHandlerServesStandalonePortalAndAPI(t *testing.T) {
	assets := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte(dashboardTestIndex)},
		"app.js":     &fstest.MapFile{Data: []byte("app")},
	}
	api := http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(response, "api")
	})
	handler, err := newDashboardHandler(assets, api, dashboardModeStandalone, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	index := httptest.NewRecorder()
	handler.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/", nil))
	if index.Code != http.StatusOK || !strings.Contains(index.Body.String(), `content="standalone"`) {
		t.Fatalf("index response = %d %q", index.Code, index.Body.String())
	}
	if cache := index.Header().Get("Cache-Control"); cache != "no-store" {
		t.Fatalf("index Cache-Control = %q", cache)
	}

	static := httptest.NewRecorder()
	handler.ServeHTTP(static, httptest.NewRequest(http.MethodGet, "/app.js", nil))
	if static.Code != http.StatusOK || static.Body.String() != "app" {
		t.Fatalf("static response = %d %q", static.Code, static.Body.String())
	}

	apiResponse := httptest.NewRecorder()
	handler.ServeHTTP(apiResponse, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if apiResponse.Code != http.StatusOK || apiResponse.Body.String() != "api" {
		t.Fatalf("API response = %d %q", apiResponse.Code, apiResponse.Body.String())
	}
}

func TestListenDashboardReportsConflictAndCanIncrement(t *testing.T) {
	// Deliberately NOT net.Listen("127.0.0.1:0"): the OS hands back a port
	// from the dynamic/ephemeral range (49152-65535 by default on Windows) —
	// the exact range Hyper-V/WinNAT periodically reserve blocks out of (see
	// dashboardPortUnavailable's WSAEACCES handling in
	// dashboard_socket_windows.go). An "occupied" port drawn from up there
	// can leave the auto-increment search below with too little headroom
	// before 65535, or walk straight into a reserved block — a real,
	// host-NAT-state-dependent flake on Windows CI (#2048), not a code bug.
	// A low, fixed port far outside that range gives the increment search a
	// wide, deterministic margin regardless of host NAT state.
	occupied, port := listenFixedTestPort(t, 23456)
	defer func() { _ = occupied.Close() }()

	if _, err := listenDashboard("127.0.0.1", dashboardPort{number: port}); err == nil || !strings.Contains(err.Error(), "--port=auto") {
		t.Fatalf("exact-port error = %v", err)
	}
	incremented, err := listenDashboard("127.0.0.1", dashboardPort{number: port, auto: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = incremented.Close() }()
	_, incrementedText, err := net.SplitHostPort(incremented.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	incrementedPort, err := strconv.Atoi(incrementedText)
	if err != nil {
		t.Fatal(err)
	}
	if incrementedPort <= port {
		t.Fatalf("auto port = %d, want greater than occupied port %d", incrementedPort, port)
	}
}

// listenFixedTestPort binds the first available port at or after base,
// scanning a small range — used instead of an ephemeral (":0") port by tests
// that exercise port-conflict/increment logic, so the chosen port stays well
// outside Windows' dynamic/ephemeral range regardless of which port the OS
// would have handed back (see the comment on
// TestListenDashboardReportsConflictAndCanIncrement).
func listenFixedTestPort(t *testing.T, base int) (net.Listener, int) {
	t.Helper()
	for port := base; port < base+20; port++ {
		l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err == nil {
			return l, port
		}
	}
	t.Fatalf("no free port available in range %d-%d", base, base+19)
	return nil, 0
}

func TestParseDashboardListen(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		wantHost string
		wantPort int
		wantErr  string
	}{
		{name: "loopback host and port", value: "127.0.0.1:9000", wantHost: "127.0.0.1", wantPort: 9000},
		{name: "non-loopback host", value: "0.0.0.0:9000", wantHost: "0.0.0.0", wantPort: 9000},
		{name: "hostname", value: "dashboard.internal:9000", wantHost: "dashboard.internal", wantPort: 9000},
		{name: "missing port", value: "0.0.0.0", wantErr: "host:port"},
		{name: "wildcard empty host", value: ":9000", wantErr: "host is required"},
		{name: "auto is not a supported port value", value: "0.0.0.0:auto", wantErr: "number from 1 through 65535"},
		{name: "port out of range", value: "0.0.0.0:70000", wantErr: "number from 1 through 65535"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host, port, err := parseDashboardListen(test.value)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("parseDashboardListen(%q) error = %v, want containing %q", test.value, err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDashboardListen(%q) = %v", test.value, err)
			}
			if host != test.wantHost || port.number != test.wantPort || port.auto {
				t.Fatalf("parseDashboardListen(%q) = (%q, %+v), want (%q, %d)", test.value, host, port, test.wantHost, test.wantPort)
			}
		})
	}
}

func TestDashboardWaitFlag(t *testing.T) {
	var wait dashboardWaitFlag
	if err := wait.Set("true"); err != nil {
		t.Fatal(err)
	}
	if wait.duration() != dashboardAttachTimeout {
		t.Fatalf("bare wait duration = %s, want %s", wait.duration(), dashboardAttachTimeout)
	}
	if err := wait.Set("2m"); err != nil {
		t.Fatal(err)
	}
	if wait.duration() != 2*time.Minute {
		t.Fatalf("explicit wait duration = %s, want 2m", wait.duration())
	}
	if err := wait.Set("0s"); err == nil {
		t.Fatal("zero wait duration accepted")
	}
}

// TestValidateDashboardListenHostFailsClosedOffLoopback mirrors
// TestValidateAPIListenFailClosedOffLoopback (internal/instance) for the
// portal's own gate (#2884): a non-loopback bind is refused unless
// api.auth.oidc is configured, and there is no insecure override. Unlike the
// API gate, api.tls is irrelevant here — the dashboard never terminates TLS
// itself (see validateDashboardListenHost's doc comment).
func TestValidateDashboardListenHostFailsClosedOffLoopback(t *testing.T) {
	authenticated := &instance.APIAuthConfig{OIDC: &instance.OIDCAuthConfig{
		Issuer:   "https://issuer.example.com",
		Audience: "api://goobers",
		Roles:    instance.OIDCRoleMapping{View: []string{"team-viewers"}},
	}}
	tests := []struct {
		name    string
		host    string
		auth    *instance.APIAuthConfig
		wantErr string
	}{
		{name: "loopback IPv4 stays valid unconfigured", host: "127.0.0.1"},
		{name: "loopback IPv6 stays valid unconfigured", host: "::1"},
		{name: "localhost stays valid unconfigured", host: "localhost"},
		{name: "non-loopback without auth is refused", host: "0.0.0.0", wantErr: "not loopback"},
		{name: "hostname bind counts as non-loopback", host: "dashboard.internal", wantErr: "not loopback"},
		{name: "non-loopback with auth configured is accepted", host: "0.0.0.0", auth: authenticated},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := &instance.Config{API: instance.APIConfig{Auth: test.auth}}
			err := validateDashboardListenHost(test.host, config)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("validateDashboardListenHost(%q) = %v, want nil", test.host, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("validateDashboardListenHost(%q) = %v, want error containing %q", test.host, err, test.wantErr)
			}
		})
	}
}

// setDashboardAPIAuth writes a valid api.auth.oidc block into instance.yaml,
// the same authenticator surface validateDashboardListenHost gates on.
func setDashboardAPIAuth(t *testing.T, root string) {
	t.Helper()
	layout := instance.NewLayout(root)
	config, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	config.API.Auth = &instance.APIAuthConfig{OIDC: &instance.OIDCAuthConfig{
		Issuer:   "https://issuer.example.com",
		Audience: "api://goobers",
		Roles:    instance.OIDCRoleMapping{View: []string{"team-viewers"}},
	}}
	if err := instance.WriteConfig(layout.ConfigFile(), config); err != nil {
		t.Fatal(err)
	}
}

// freeNonLoopbackTestPort finds a currently-free port by binding loopback
// (like freeLoopbackAddress) and handing back just the port number, for
// tests that then bind a non-loopback host at that port explicitly.
func freeNonLoopbackTestPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func useLoopbackDashboardTestListener(t *testing.T) {
	t.Helper()
	original := listenDashboardTCP
	listenDashboardTCP = func(network, address string) (net.Listener, error) {
		_, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		return net.Listen(network, net.JoinHostPort("127.0.0.1", port))
	}
	t.Cleanup(func() {
		listenDashboardTCP = original
	})
}

// TestRunDashboardContextRefusesNonLoopbackListenWithoutAuth pins the
// fail-closed CLI behaviour (#2884): --listen to a non-loopback host is
// refused at startup, before anything is served, when instance.yaml has no
// api.auth configured — the tier-1 default posture.
func TestRunDashboardContextRefusesNonLoopbackListenWithoutAuth(t *testing.T) {
	root := initDemo(t)
	port := freeNonLoopbackTestPort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stderr bytes.Buffer
	code := runDashboardContext(ctx, dashboardTestArgs(t, "--listen=0.0.0.0:"+strconv.Itoa(port), "--no-open", root), io.Discard, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, stderr = %q, want 2", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "api.auth") || !strings.Contains(stderr.String(), "not loopback") {
		t.Fatalf("stderr = %q, want a fail-closed api.auth/loopback refusal", stderr.String())
	}
}

// TestRunDashboardContextAcceptsNonLoopbackListenWithAuth is the accepted
// counterpart (#2884): once api.auth.oidc is configured, --listen to a
// non-loopback host is allowed to bind and actually serves the portal.
func TestRunDashboardContextAcceptsNonLoopbackListenWithAuth(t *testing.T) {
	root := initDemo(t)
	setDashboardAPIAuth(t, root)
	// Preserve the configured non-loopback host through validation and URL
	// publication, but never expose the disposable test binary off-loopback.
	// Windows Defender otherwise asks for a persistent firewall decision for
	// the temporary goobers.test.exe.
	useLoopbackDashboardTestListener(t)
	port := freeNonLoopbackTestPort(t)
	ctx, cancel := context.WithCancel(context.Background())
	started := &dashboardURLWriter{url: make(chan string, 1)}
	done := make(chan int, 1)
	args := dashboardTestArgs(t, "--listen=0.0.0.0:"+strconv.Itoa(port), "--no-open", root)
	go func() {
		done <- runDashboardContext(ctx, args, started, io.Discard)
	}()

	var address string
	select {
	case address = <-started.url:
	case code := <-done:
		t.Fatalf("dashboard exited before startup: code = %d", code)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for dashboard URL")
	}
	if !strings.HasPrefix(address, "http://0.0.0.0:") {
		cancel()
		t.Fatalf("dashboard URL = %q, want the configured non-loopback host", address)
	}
	// 0.0.0.0 binds every interface including loopback, so a loopback client
	// exercises the same listener without depending on outbound reachability
	// to the literal 0.0.0.0 destination.
	loopbackAddress := "http://127.0.0.1:" + strings.TrimPrefix(address, "http://0.0.0.0:")
	// Unauthenticated: the standalone handler enforces api.auth even when the
	// bind itself was permitted, so a bare request is refused.
	response, err := http.Get(loopbackAddress + "api/v1/health")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		cancel()
		t.Fatalf("unauthenticated API status = %d, want 401", response.StatusCode)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("dashboard exit code = %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dashboard did not stop after cancellation")
	}
}

// TestStandaloneDashboardAPIEnforcesConfiguredAuthenticator confirms
// api.auth is a real, request-level gate on the standalone handler, not just
// a config-presence check that only feeds validateDashboardListenHost
// (#2884): the same block that opens the non-loopback bind must also make
// the handler it opens actually refuse unauthenticated requests.
func TestStandaloneDashboardAPIEnforcesConfiguredAuthenticator(t *testing.T) {
	root := initDemo(t)
	layout := instance.NewLayout(root)
	config, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	config.API.Auth = &instance.APIAuthConfig{OIDC: &instance.OIDCAuthConfig{
		Issuer:   "https://issuer.example.com",
		Audience: "api://goobers",
		Roles:    instance.OIDCRoleMapping{View: []string{"team-viewers"}},
	}}

	api, err := standaloneDashboardAPI(layout, config, log.New(io.Discard, "", 0), true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := api.close(); err != nil {
			t.Fatal(err)
		}
	}()

	response := httptest.NewRecorder()
	api.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, httpapi.HealthPath, nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request status = %d, want 401", response.Code)
	}
}

// TestStandaloneDashboardAPIWithholdsRevealOffLoopback locks in the guard
// added alongside #2884's --listen gate: the reveal-in-Finder action shells
// out on the dashboard process's own machine (#2306), which is only
// guaranteed to be the requesting user's machine when the listener is
// loopback. `goobers up` already withholds the same capability from the API
// once its own listen address is non-loopback
// (docs/design/portal-reveal-remote-posture.md); standaloneDashboardAPI's
// new loopback parameter must do the same for the dashboard's handler,
// mirrored through the portal/config capabilities.revealRun flag the portal
// frontend reads to decide whether to render the button.
func TestStandaloneDashboardAPIWithholdsRevealOffLoopback(t *testing.T) {
	for _, test := range []struct {
		name           string
		loopback       bool
		wantRevealable bool
	}{
		{name: "loopback keeps reveal available", loopback: true, wantRevealable: true},
		{name: "non-loopback withholds reveal", loopback: false, wantRevealable: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := initDemo(t)
			layout := instance.NewLayout(root)
			config, err := instance.LoadConfig(layout.ConfigFile())
			if err != nil {
				t.Fatal(err)
			}
			api, err := standaloneDashboardAPI(layout, config, log.New(io.Discard, "", 0), test.loopback)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := api.close(); err != nil {
					t.Fatal(err)
				}
			}()

			response := httptest.NewRecorder()
			api.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, httpapi.PortalConfigPath, nil))
			if response.Code != http.StatusOK {
				t.Fatalf("portal config status = %d, body = %q", response.Code, response.Body.String())
			}
			var decoded struct {
				Capabilities struct {
					RevealRun bool `json:"revealRun"`
				} `json:"capabilities"`
			}
			if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.Capabilities.RevealRun != test.wantRevealable {
				t.Fatalf("capabilities.revealRun = %t, want %t", decoded.Capabilities.RevealRun, test.wantRevealable)
			}
		})
	}
}

func TestPrepareDashboardAPIAttachesOnlyToLiveDaemon(t *testing.T) {
	root := initDemo(t)
	layout := instance.NewLayout(root)
	daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != httpapi.HealthPath {
			http.NotFound(response, request)
			return
		}
		if err := json.NewEncoder(response).Encode(readservice.Health{
			APIVersion:    readservice.APIVersion,
			SchemaVersion: readservice.SchemaVersion,
			Ready:         true,
			Healthy:       true,
		}); err != nil {
			t.Errorf("encode health response: %v", err)
		}
	}))
	defer daemon.Close()
	setAPIListenAddress(t, root, strings.TrimPrefix(daemon.URL, "http://"))
	config, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	release, err := acquireDaemonLock(filepath.Join(layout.SchedulerDir(), "up.lock"), root, instance.DefaultDaemonLivenessTimeout, nil)
	if err != nil {
		t.Fatal(err)
	}

	api, err := prepareDashboardAPI(context.Background(), layout, config, log.New(io.Discard, "", 0), true, 0)
	if err != nil {
		release()
		t.Fatal(err)
	}
	if api.mode != dashboardModeDaemon {
		release()
		t.Fatalf("mode = %q, want daemon", api.mode)
	}
	proxied := httptest.NewRecorder()
	api.handler.ServeHTTP(proxied, httptest.NewRequest(http.MethodGet, httpapi.HealthPath, nil))
	if proxied.Code != http.StatusOK {
		release()
		t.Fatalf("proxied health status = %d", proxied.Code)
	}
	release()
	if err := api.close(); err != nil {
		t.Fatal(err)
	}

	standalone, err := prepareDashboardAPI(context.Background(), layout, config, log.New(io.Discard, "", 0), true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := standalone.close(); err != nil {
			t.Errorf("close standalone API: %v", err)
		}
	}()
	if standalone.mode != dashboardModeStandalone {
		t.Fatalf("mode = %q, want standalone", standalone.mode)
	}
	response := httptest.NewRecorder()
	standalone.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, httpapi.HealthPath, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("standalone health status = %d, body = %q", response.Code, response.Body.String())
	}
}

func TestPrepareDashboardAPIWaitsForConcurrentlyStartingDaemon(t *testing.T) {
	root := initDemo(t)
	layout := instance.NewLayout(root)
	daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != httpapi.HealthPath {
			http.NotFound(response, request)
			return
		}
		_ = json.NewEncoder(response).Encode(readservice.Health{
			APIVersion:    readservice.APIVersion,
			SchemaVersion: readservice.SchemaVersion,
			Ready:         true,
			Healthy:       true,
		})
	}))
	defer daemon.Close()
	setAPIListenAddress(t, root, strings.TrimPrefix(daemon.URL, "http://"))
	config, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}

	type result struct {
		api dashboardAPI
		err error
	}
	done := make(chan result, 1)
	go func() {
		api, err := prepareDashboardAPI(context.Background(), layout, config, log.New(io.Discard, "", 0), true, 2*time.Second)
		done <- result{api: api, err: err}
	}()
	select {
	case result := <-done:
		t.Fatalf("dashboard stopped waiting before daemon startup: %v", result.err)
	case <-time.After(150 * time.Millisecond):
	}

	release, err := acquireDaemonLock(filepath.Join(layout.SchedulerDir(), "up.lock"), root, instance.DefaultDaemonLivenessTimeout, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		defer func() {
			if err := result.api.close(); err != nil {
				t.Error(err)
			}
		}()
		if result.api.mode != dashboardModeDaemon {
			t.Fatalf("mode = %q, want daemon", result.api.mode)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dashboard did not attach after daemon startup")
	}
}

func TestPrepareDashboardAPIWaitsForLockReacquisitionDuringStartup(t *testing.T) {
	root := initDemo(t)
	layout := instance.NewLayout(root)
	lockPath := filepath.Join(layout.SchedulerDir(), "up.lock")
	release, err := acquireDaemonLock(lockPath, root, instance.DefaultDaemonLivenessTimeout, nil)
	if err != nil {
		t.Fatal(err)
	}
	lockReleased := make(chan struct{})
	var releaseOnce sync.Once
	daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != httpapi.HealthPath {
			http.NotFound(response, request)
			return
		}
		releaseOnce.Do(func() {
			release()
			close(lockReleased)
		})
		_ = json.NewEncoder(response).Encode(readservice.Health{
			APIVersion:    readservice.APIVersion,
			SchemaVersion: readservice.SchemaVersion,
			Ready:         true,
			Healthy:       true,
		})
	}))
	defer daemon.Close()
	setAPIListenAddress(t, root, strings.TrimPrefix(daemon.URL, "http://"))
	config, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}

	type result struct {
		api dashboardAPI
		err error
	}
	done := make(chan result, 1)
	go func() {
		api, err := prepareDashboardAPI(context.Background(), layout, config, log.New(io.Discard, "", 0), true, 2*time.Second)
		done <- result{api: api, err: err}
	}()
	select {
	case <-lockReleased:
	case <-time.After(time.Second):
		t.Fatal("dashboard did not probe the first daemon")
	}
	select {
	case result := <-done:
		t.Fatalf("dashboard selected a mode after the daemon lock disappeared: mode=%q err=%v", result.api.mode, result.err)
	case <-time.After(150 * time.Millisecond):
	}

	releaseAgain, err := acquireDaemonLock(lockPath, root, instance.DefaultDaemonLivenessTimeout, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseAgain()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.api.mode != dashboardModeDaemon {
			t.Fatalf("mode = %q, want daemon", result.api.mode)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dashboard did not attach after daemon lock reacquisition")
	}
}

func TestPrepareDashboardAPIWaitForDaemonTimesOut(t *testing.T) {
	root := initDemo(t)
	layout := instance.NewLayout(root)
	config, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	_, err = prepareDashboardAPI(context.Background(), layout, config, log.New(io.Discard, "", 0), true, 150*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "up.lock") {
		t.Fatalf("wait error = %v, want timeout naming up.lock", err)
	}
}

func TestPrepareDashboardAPIRefusesAuthenticatedDaemonAttach(t *testing.T) {
	root := initDemo(t)
	layout := instance.NewLayout(root)
	config, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	config.API.Auth = &instance.APIAuthConfig{OIDC: &instance.OIDCAuthConfig{
		Issuer:   "https://issuer.example.com",
		Audience: "api://goobers",
		Roles:    instance.OIDCRoleMapping{View: []string{"team-viewers"}},
	}}
	release, err := acquireDaemonLock(filepath.Join(layout.SchedulerDir(), "up.lock"), root, instance.DefaultDaemonLivenessTimeout, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	// The refusal must name the cause up front — not probe a 401ing health
	// endpoint until the attach timeout.
	start := time.Now()
	_, err = prepareDashboardAPI(context.Background(), layout, config, log.New(io.Discard, "", 0), true, 0)
	if err == nil || !strings.Contains(err.Error(), "bearer token") {
		t.Fatalf("prepareDashboardAPI error = %v, want bearer-token refusal", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("authenticated-daemon refusal took %s, want fail-fast", elapsed)
	}
}

func TestPrepareDashboardAPIProbesTLSDaemonOverHTTPS(t *testing.T) {
	root := initDemo(t)
	layout := instance.NewLayout(root)
	// This test deliberately makes the probe reject the daemon's self-signed
	// certificate, so the server-side handshake failure below ("remote
	// error: tls: bad certificate") is the expected outcome, not a fixture
	// bug (#4366) — but net/http's default server logs it to os.Stderr
	// regardless, which local-ci's log-scanning classifiers have mistaken
	// for a genuine failure signature (#4362/#4367). Route it to a discard
	// logger instead of leaving Config.ErrorLog nil (httptest.NewServer's
	// default is log.Default(), i.e. stderr), so this expected rejection
	// never pollutes local-ci output. Deliberately not asserted on: the
	// server logs it from its own connection goroutine, asynchronously with
	// respect to the client returning below, so capturing and asserting on
	// it would add exactly the kind of timing-dependent check this fixture
	// fix exists to avoid.
	daemon := httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusOK)
	}))
	daemon.Config.ErrorLog = log.New(io.Discard, "", 0)
	daemon.StartTLS()
	defer daemon.Close()
	setAPIListenAddress(t, root, strings.TrimPrefix(daemon.URL, "https://"))
	config, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	config.API.TLS = &instance.APITLSConfig{CertFile: "cert.pem", KeyFile: "key.pem"}
	release, err := acquireDaemonLock(filepath.Join(layout.SchedulerDir(), "up.lock"), root, instance.DefaultDaemonLivenessTimeout, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	originalTimeout := dashboardAttachTimeout
	dashboardAttachTimeout = 3 * time.Second
	defer func() { dashboardAttachTimeout = originalTimeout }()

	// The probe speaks HTTPS (a plain http:// probe against a TLS listener
	// would report a protocol error, never a certificate one) and fails fast
	// on the untrusted test certificate instead of spinning to the attach
	// timeout, whose message says "unavailable" rather than "does not trust".
	_, err = prepareDashboardAPI(context.Background(), layout, config, log.New(io.Discard, "", 0), true, 0)
	if err == nil || !strings.Contains(err.Error(), "does not trust") {
		t.Fatalf("prepareDashboardAPI error = %v, want fail-fast certificate trust error", err)
	}
}

func TestPrepareDashboardAPIAttachesWhenDaemonTicksAreStale(t *testing.T) {
	root := initDemo(t)
	layout := instance.NewLayout(root)
	lastTick := time.Now().UTC().Add(-3 * time.Minute)
	lastTickAgeMillis := int64((3 * time.Minute) / time.Millisecond)
	daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != httpapi.HealthPath {
			http.NotFound(response, request)
			return
		}
		if err := json.NewEncoder(response).Encode(readservice.Health{
			APIVersion:    readservice.APIVersion,
			SchemaVersion: readservice.SchemaVersion,
			Ready:         true,
			Healthy:       false,
			Freshness: readservice.Freshness{
				ObservedAt:          time.Now().UTC(),
				LastSchedulerTickAt: &lastTick,
				LastTickAgeMillis:   &lastTickAgeMillis,
			},
		}); err != nil {
			t.Errorf("encode health response: %v", err)
		}
	}))
	defer daemon.Close()
	setAPIListenAddress(t, root, strings.TrimPrefix(daemon.URL, "http://"))
	config, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(layout.SchedulerDir(), "up.lock")
	release, err := acquireDaemonLock(lockPath, root, instance.DefaultDaemonLivenessTimeout, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := daemonstate.Refresh(lockPath, time.Now().Add(-3*time.Minute)); err != nil {
		t.Fatal(err)
	}

	api, err := prepareDashboardAPI(context.Background(), layout, config, log.New(io.Discard, "", 0), true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := api.close(); err != nil {
			t.Errorf("close dashboard API: %v", err)
		}
	}()
	if api.mode != dashboardModeDaemon {
		t.Fatalf("mode = %q, want daemon for responsive stale daemon", api.mode)
	}
	handler, err := newDashboardHandler(
		fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte(dashboardTestIndex)}},
		api.handler,
		api.mode,
		root,
	)
	if err != nil {
		t.Fatal(err)
	}
	index := httptest.NewRecorder()
	handler.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/", nil))
	if index.Code != http.StatusOK || !strings.Contains(index.Body.String(), `content="daemon"`) {
		t.Fatalf("index response = %d %q", index.Code, index.Body.String())
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, httpapi.HealthPath, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("health status = %d, body = %q", response.Code, response.Body.String())
	}
	var health readservice.Health
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if health.Healthy {
		t.Fatal("dashboard masked the stale scheduler heartbeat")
	}
	if health.Freshness.LastTickAgeMillis == nil || *health.Freshness.LastTickAgeMillis != lastTickAgeMillis {
		t.Fatalf("last tick age = %v, want %d", health.Freshness.LastTickAgeMillis, lastTickAgeMillis)
	}
}

func TestDashboardHandlerServesInstanceAssets(t *testing.T) {
	root := t.TempDir()
	assetsDir := filepath.Join(root, "assets")
	if err := os.MkdirAll(assetsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logoPath := filepath.Join(assetsDir, "logo.svg")
	if err := os.WriteFile(logoPath, []byte("<svg/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	handler, err := newDashboardHandler(
		fstest.MapFS{
			"index.html":       &fstest.MapFile{Data: []byte(dashboardTestIndex)},
			"assets/bundle.js": &fstest.MapFile{Data: []byte("//embedded-bundle")},
		},
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		dashboardModeStandalone,
		root,
	)
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/assets/logo.svg", nil))
	if response.Code != http.StatusOK || response.Body.String() != "<svg/>" {
		t.Fatalf("asset response = %d %q", response.Code, response.Body.String())
	}

	// An /assets/ path with no instance-root override must fall through to the
	// embedded bundle — the portal ships its own /assets/index-*.js|css there,
	// so instance co-branding must not shadow it.
	bundle := httptest.NewRecorder()
	handler.ServeHTTP(bundle, httptest.NewRequest(http.MethodGet, "/assets/bundle.js", nil))
	if bundle.Code != http.StatusOK || bundle.Body.String() != "//embedded-bundle" {
		t.Fatalf("embedded bundle response = %d %q, want 200 %q", bundle.Code, bundle.Body.String(), "//embedded-bundle")
	}

	traversal := httptest.NewRecorder()
	handler.ServeHTTP(traversal, httptest.NewRequest(http.MethodGet, "/assets/../dashboard.go", nil))
	if traversal.Code != http.StatusNotFound {
		t.Fatalf("traversal status = %d, want 404", traversal.Code)
	}
}

func TestDashboardCancellationWhileAttachingExitsCleanlyBeforeURL(t *testing.T) {
	root := initDemo(t)
	layout := instance.NewLayout(root)
	requestStarted := make(chan struct{})
	var requestOnce sync.Once
	daemon := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		requestOnce.Do(func() { close(requestStarted) })
		<-request.Context().Done()
	}))
	defer daemon.Close()
	setAPIListenAddress(t, root, strings.TrimPrefix(daemon.URL, "http://"))
	release, err := acquireDaemonLock(filepath.Join(layout.SchedulerDir(), "up.lock"), root, instance.DefaultDaemonLivenessTimeout, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	args := dashboardTestArgs(t, "--port=auto", "--no-open", root)
	go func() {
		done <- runDashboardContext(ctx, args, &stdout, &stderr)
	}()

	select {
	case <-requestStarted:
	case code := <-done:
		t.Fatalf("dashboard exited before cancellation: code = %d, stderr = %q", code, stderr.String())
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for dashboard to attach")
	}
	cancel()

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("dashboard exit code = %d, stderr = %q", code, stderr.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dashboard did not stop after cancellation")
	}
	if stdout.Len() != 0 {
		t.Fatalf("dashboard printed URL before startup: %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("dashboard reported cancellation as an error: %q", stderr.String())
	}
	running, _, err := inspectDaemonLock(filepath.Join(layout.SchedulerDir(), "up.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if !running {
		t.Fatal("dashboard cancellation disturbed the live daemon lock")
	}
}

func TestDashboardCancellationDuringBrowserLaunchLeavesLiveDaemonRunning(t *testing.T) {
	root := initDemo(t)
	layout := instance.NewLayout(root)
	daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != httpapi.HealthPath {
			http.NotFound(response, request)
			return
		}
		if err := json.NewEncoder(response).Encode(readservice.Health{
			APIVersion:    readservice.APIVersion,
			SchemaVersion: readservice.SchemaVersion,
			Ready:         true,
			Healthy:       true,
		}); err != nil {
			t.Errorf("encode health response: %v", err)
		}
	}))
	defer daemon.Close()
	setAPIListenAddress(t, root, strings.TrimPrefix(daemon.URL, "http://"))
	release, err := acquireDaemonLock(filepath.Join(layout.SchedulerDir(), "up.lock"), root, instance.DefaultDaemonLivenessTimeout, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	launcherStarted := make(chan string, 1)
	originalLauncher := launchDashboardBrowser
	launchDashboardBrowser = func(ctx context.Context, address string) error {
		launcherStarted <- address
		<-ctx.Done()
		return ctx.Err()
	}
	defer func() { launchDashboardBrowser = originalLauncher }()

	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	args := dashboardTestArgs(t, "--port=auto", root)
	go func() {
		done <- runDashboardContext(ctx, args, &stdout, &stderr)
	}()

	var dashboardAddress string
	select {
	case dashboardAddress = <-launcherStarted:
	case code := <-done:
		t.Fatalf("dashboard exited before browser launch: code = %d, stderr = %q", code, stderr.String())
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for browser launch")
	}
	cancel()

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("dashboard exit code = %d, stderr = %q", code, stderr.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dashboard did not stop after cancellation")
	}
	if stderr.String() != "dashboard: mode=daemon\n" {
		t.Fatalf("dashboard mode output = %q, want daemon mode", stderr.String())
	}
	if stdout.String() != dashboardAddress+"\n" {
		t.Fatalf("dashboard output = %q, want %q", stdout.String(), dashboardAddress+"\n")
	}
	client := &http.Client{Timeout: time.Second}
	if response, err := client.Get(dashboardAddress); err == nil {
		_ = response.Body.Close()
		t.Fatal("dashboard server remains available after cancellation")
	}
	running, _, err := inspectDaemonLock(filepath.Join(layout.SchedulerDir(), "up.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if !running {
		t.Fatal("dashboard cancellation disturbed the live daemon lock")
	}
	response, err := client.Get(daemon.URL + httpapi.HealthPath)
	if err != nil {
		t.Fatalf("live daemon stopped after dashboard cancellation: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("live daemon health status = %d", response.StatusCode)
	}
}

func TestDashboardAttachesToLiveDaemonWithEphemeralAPIAddress(t *testing.T) {
	root := initDeterministicDemo(t)
	setAPIListenAddress(t, root, "127.0.0.1:0")
	layout := instance.NewLayout(root)
	ctx, cancel := context.WithCancel(context.Background())
	started := &daemonStartedWriter{started: make(chan struct{})}
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runUpContext(ctx, []string{"--quiet", root}, started, &stderr)
	}()
	t.Cleanup(cancel)

	select {
	case <-started.started:
	case code := <-done:
		t.Fatalf("daemon exited before startup: code = %d, stderr = %q", code, stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for daemon startup")
	}

	published, err := os.ReadFile(filepath.Join(layout.SchedulerDir(), daemonAPIAddressFileName))
	if err != nil {
		t.Fatal(err)
	}
	address := strings.TrimSpace(string(published))
	_, port, err := net.SplitHostPort(address)
	if err != nil || port == "0" {
		t.Fatalf("published daemon API address = %q, parse error = %v", address, err)
	}

	config, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	api, err := prepareDashboardAPI(context.Background(), layout, config, log.New(io.Discard, "", 0), true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if api.mode != dashboardModeDaemon {
		t.Fatalf("mode = %q, want daemon", api.mode)
	}
	response := httptest.NewRecorder()
	api.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, httpapi.HealthPath, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("proxied health status = %d, body = %q", response.Code, response.Body.String())
	}
	if err := api.close(); err != nil {
		t.Fatal(err)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("daemon exit code = %d, stderr = %q", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for daemon shutdown")
	}
	if _, err := os.Stat(filepath.Join(layout.SchedulerDir(), daemonAPIAddressFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("daemon API address file remains after shutdown: %v", err)
	}
}

func TestStandaloneDashboardAPILeavesInstanceUnchanged(t *testing.T) {
	root := initDemo(t)
	layout := instance.NewLayout(root)
	config, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotDashboardInstance(t, root)

	api, err := standaloneDashboardAPI(layout, config, log.New(io.Discard, "", 0), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := api.close(); err != nil {
		t.Fatal(err)
	}

	after := snapshotDashboardInstance(t, root)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("standalone dashboard changed instance files\nbefore: %#v\nafter:  %#v", before, after)
	}
}

func snapshotDashboardInstance(t *testing.T, root string) map[string]string {
	t.Helper()
	files := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[relative] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestDashboardAssetFSRequiresIndex(t *testing.T) {
	dir := t.TempDir()
	if _, err := dashboardAssetFS(dir); err == nil {
		t.Fatal("dashboardAssetFS accepted a directory without index.html")
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(dashboardTestIndex), 0o644); err != nil {
		t.Fatal(err)
	}
	assets, err := dashboardAssetFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Stat(assets, "index.html"); err != nil {
		t.Fatal(err)
	}
}

func TestRunDirectoryRevealerResolvesTheJournalDirectory(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	runDir := filepath.Join(layout.RunsDir(), "run-1")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	originalLauncher := launchRunDirectory
	t.Cleanup(func() { launchRunDirectory = originalLauncher })
	var launched string
	launchRunDirectory = func(_ context.Context, path string) error {
		launched = path
		return nil
	}

	if err := runDirectoryRevealer(layout)(context.Background(), "run-1"); err != nil {
		t.Fatal(err)
	}
	if launched != runDir {
		t.Fatalf("launched path = %q, want %q", launched, runDir)
	}
}

func TestDashboardNoOpenPrintsURLAndStopsCleanly(t *testing.T) {
	root := initDemo(t)
	ctx, cancel := context.WithCancel(context.Background())
	started := &dashboardURLWriter{url: make(chan string, 1)}
	done := make(chan int, 1)
	var stderr bytes.Buffer
	originalLauncher := launchDashboardBrowser
	browserCalled := false
	launchDashboardBrowser = func(context.Context, string) error {
		browserCalled = true
		return nil
	}
	defer func() { launchDashboardBrowser = originalLauncher }()

	args := dashboardTestArgs(t, "--port=auto", "--no-open", root)
	go func() {
		done <- runDashboardContext(ctx, args, started, &stderr)
	}()

	var address string
	select {
	case address = <-started.url:
	case code := <-done:
		t.Fatalf("dashboard exited before startup: code = %d", code)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for dashboard URL")
	}
	if browserCalled {
		t.Fatal("--no-open launched a browser")
	}
	response, err := http.Get(address)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		cancel()
		t.Fatal(errors.Join(readErr, closeErr))
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `content="standalone"`) {
		cancel()
		t.Fatalf("portal response = %d %q", response.StatusCode, body)
	}

	events, err := http.Get(strings.TrimSuffix(address, "/") + httpapi.EventsPath)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if events.StatusCode != http.StatusOK {
		_ = events.Body.Close()
		cancel()
		t.Fatalf("events status = %d", events.StatusCode)
	}
	cancel()
	select {
	case code := <-done:
		_ = events.Body.Close()
		if code != 0 {
			t.Fatalf("dashboard exit code = %d", code)
		}
	case <-time.After(2 * time.Second):
		_ = events.Body.Close()
		t.Fatal("dashboard did not stop after cancellation")
	}
	if stderr.String() != "dashboard: mode=standalone\n" {
		t.Fatalf("dashboard mode output = %q, want standalone mode", stderr.String())
	}
}
