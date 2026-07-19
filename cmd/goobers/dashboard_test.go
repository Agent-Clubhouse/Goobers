package main

import (
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
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readservice"
)

const dashboardTestIndex = `<!doctype html><html><head><meta name="goobers-dashboard-mode" content="daemon" /></head><body>portal</body></html>`

type dashboardURLWriter struct {
	once sync.Once
	url  chan string
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
	handler, err := newDashboardHandler(assets, api, dashboardModeStandalone)
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
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = occupied.Close() }()
	_, portText, err := net.SplitHostPort(occupied.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := listenDashboard(dashboardPort{number: port}); err == nil || !strings.Contains(err.Error(), "--port=auto") {
		t.Fatalf("exact-port error = %v", err)
	}
	if port == 65535 {
		t.Skip("ephemeral port leaves no increment range")
	}
	incremented, err := listenDashboard(dashboardPort{number: port, auto: true})
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
	release, err := acquireDaemonLock(filepath.Join(layout.SchedulerDir(), "up.lock"), root)
	if err != nil {
		t.Fatal(err)
	}

	api, err := prepareDashboardAPI(context.Background(), layout, config, log.New(io.Discard, "", 0))
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

	standalone, err := prepareDashboardAPI(context.Background(), layout, config, log.New(io.Discard, "", 0))
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

func TestDashboardNoOpenPrintsURLAndStopsCleanly(t *testing.T) {
	root := initDemo(t)
	ctx, cancel := context.WithCancel(context.Background())
	started := &dashboardURLWriter{url: make(chan string, 1)}
	done := make(chan int, 1)
	originalLauncher := launchDashboardBrowser
	browserCalled := false
	launchDashboardBrowser = func(string) error {
		browserCalled = true
		return nil
	}
	defer func() { launchDashboardBrowser = originalLauncher }()

	go func() {
		done <- runDashboardContext(ctx, []string{"--port=auto", "--no-open", root}, started, io.Discard)
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
}
