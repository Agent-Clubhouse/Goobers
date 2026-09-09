package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
)

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
