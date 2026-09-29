package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"

	"go.yaml.in/yaml/v3"
)

func TestLocalDaemonAPIBaseRejectsWildcardTLSAddress(t *testing.T) {
	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	configureWildcardTLSAPI(t, layout)

	_, err := localDaemonAPIBase(layout)
	if !errors.Is(err, errWildcardTLSDaemonAPI) {
		t.Fatalf("localDaemonAPIBase() error = %v, want wildcard TLS sentinel", err)
	}
}

func configureWildcardTLSAPI(t *testing.T, layout instance.Layout) {
	t.Helper()
	config, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	config.API = instance.APIConfig{
		Listen: "0.0.0.0:8080",
		TLS:    &instance.APITLSConfig{CertFile: "/tls/server.crt", KeyFile: "/tls/server.key"},
		Auth: &instance.APIAuthConfig{OIDC: &instance.OIDCAuthConfig{
			Issuer:   "https://issuer.example",
			Audience: "goobers",
			Roles:    instance.OIDCRoleMapping{Operate: []string{"operator"}},
		}},
	}
	data, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.ConfigFile(), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.SchedulerDir(), daemonAPIAddressFileName), []byte("[::]:8080\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWildcardDaemonAPIAddress(t *testing.T) {
	for _, address := range []string{"0.0.0.0:8080", "[::]:8080", ":8080"} {
		if !wildcardDaemonAPIAddress(address) {
			t.Errorf("wildcardDaemonAPIAddress(%q) = false", address)
		}
	}
	for _, address := range []string{"127.0.0.1:8080", "[::1]:8080", "daemon.example:8080"} {
		if wildcardDaemonAPIAddress(address) {
			t.Errorf("wildcardDaemonAPIAddress(%q) = true", address)
		}
	}
}

func TestCancelWildcardTLSSelectsFileDelegation(t *testing.T) {
	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	configureWildcardTLSAPI(t, layout)
	release, err := acquireDaemonLock(filepath.Join(layout.SchedulerDir(), "up.lock"), root, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	var stderr bytes.Buffer
	handled, fileFallback, code := tryLocalAPICancel(layout, "run-1", "", false, &bytes.Buffer{}, &stderr)
	if handled || !fileFallback || code != 0 {
		t.Fatalf("handled=%t fileFallback=%t code=%d stderr=%q", handled, fileFallback, code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "using same-root file delegation") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestCancelWildcardTLSWithRequestIDRequiresAPI(t *testing.T) {
	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	configureWildcardTLSAPI(t, layout)
	release, err := acquireDaemonLock(filepath.Join(layout.SchedulerDir(), "up.lock"), root, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	var stderr bytes.Buffer
	handled, fileFallback, code := tryLocalAPICancel(layout, "run-1", "delivery", false, &bytes.Buffer{}, &stderr)
	if !handled || fileFallback || code != 2 {
		t.Fatalf("handled=%t fileFallback=%t code=%d stderr=%q", handled, fileFallback, code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "resolve daemon API") || strings.Contains(stderr.String(), "using same-root file delegation") {
		t.Fatalf("stderr = %q, want API resolution error without file fallback", stderr.String())
	}
}

func TestRunCancelAutomaticallyUsesLocalAPI(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "accepted"
		if fail {
			name = "unavailable does not delegate"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv(remoteDaemonAPIEnv, "")
			t.Setenv("GOOBERS_API_TOKEN", "operator-token")
			var requests atomic.Int32
			root, _ := interventionCLIFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if serveRemoteRootFixture(w, r) {
					return
				}
				requests.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/runs/local-cancel-1/cancel" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get(httpapi.HeaderIdempotencyKey) == "" || r.Header.Get("Authorization") != "Bearer operator-token" {
					t.Error("missing idempotency key or authentication")
				}
				if fail {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(httpapi.CancelRunResult{Code: httpapi.CancelCodeRequested})
			})
			layout := instance.NewLayout(root)
			writeFileContent(t, filepath.Join(root, instance.RootIdentityFileName), "0123456789abcdef0123456789abcdef\n")
			// Local terminal state must not override the live daemon's authority.
			createTelemetryRetentionRun(t, layout, "local-cancel-1", time.Now())
			release, err := acquireDaemonLock(filepath.Join(layout.SchedulerDir(), "up.lock"), root, time.Minute, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			code, stdout, stderr := runArgs(t, "run", "cancel", "local-cancel-1", root)
			if requests.Load() != 1 {
				t.Fatalf("requests = %d; code=%d stdout=%q stderr=%q", requests.Load(), code, stdout, stderr)
			}
			if (code == 0) == fail {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			if !fail && !strings.Contains(stdout, "requested cancellation") {
				t.Fatalf("stdout = %q", stdout)
			}
			if _, err := os.Stat(filepath.Join(layout.SchedulerDir(), "pending-cancels")); !os.IsNotExist(err) {
				t.Fatalf("unexpected file delegation: %v", err)
			}
		})
	}
}

func TestRequestedDaemonAPIExplicitFallback(t *testing.T) {
	t.Setenv(remoteDaemonAPIEnv, "http://127.0.0.1:1234")
	if endpoint, err := requestedDaemonAPI("", true); err != nil || endpoint != "" {
		t.Fatalf("explicit fallback did not override environment: %q %v", endpoint, err)
	}
	if _, err := requestedDaemonAPI("http://127.0.0.1:1234", true); err == nil {
		t.Fatal("accepted conflicting --api and --no-api")
	}
}

func TestLocalAPIMutationsRejectAnotherInstance(t *testing.T) {
	for _, command := range []string{"trigger", "cancel"} {
		t.Run(command, func(t *testing.T) {
			t.Setenv(remoteDaemonAPIEnv, "")
			var mutations atomic.Int32
			root, _ := interventionCLIFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if serveRemoteRootFixture(w, r) {
					return
				}
				mutations.Add(1)
				http.Error(w, "unexpected mutation", http.StatusInternalServerError)
			})
			writeFileContent(t, filepath.Join(root, instance.RootIdentityFileName), "ffffffffffffffffffffffffffffffff\n")
			layout := instance.NewLayout(root)
			createTelemetryRetentionRun(t, layout, "local-cancel-1", time.Now())
			release, err := acquireDaemonLock(filepath.Join(layout.SchedulerDir(), "up.lock"), root, time.Minute, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			args := []string{"run", "--no-wait", "default-implement", root}
			if command == "cancel" {
				args = []string{"run", "cancel", "local-cancel-1", root}
			}
			code, _, stderr := runArgs(t, args...)
			if code != 2 || mutations.Load() != 0 || !strings.Contains(stderr, "another instance") {
				t.Fatalf("code=%d mutations=%d stderr=%q", code, mutations.Load(), stderr)
			}
		})
	}
}
