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

func TestRunAutomaticallyUsesLocalTriggerAPI(t *testing.T) {
	for _, mode := range []string{"no-wait", "completed", "rejected", "status-unavailable"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv(remoteDaemonAPIEnv, "")
			t.Setenv("GOOBERS_API_TOKEN", "operator-token")
			var posts, gets atomic.Int32
			root, _ := interventionCLIFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if serveRemoteRootFixture(w, r) {
					return
				}
				if r.Header.Get("Authorization") != "Bearer operator-token" {
					t.Error("missing authenticated request")
				}
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPost && r.URL.Path == "/api/v1/triggers" {
					posts.Add(1)
					if r.Header.Get(httpapi.HeaderIdempotencyKey) != "delivery" {
						t.Error("missing retry key")
					}
					_ = json.NewEncoder(w).Encode(httpapi.TriggerResponse{AcceptanceID: "trigger-test", State: "accepted"})
					return
				}
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/triggers/trigger-test" {
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
				}
				count := gets.Add(1)
				if mode == "status-unavailable" {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
					return
				}
				status := httpapi.TriggerStatusResponse{AcceptanceID: "trigger-test", State: "dispatching", AcceptedAt: time.Now()}
				if count > 1 {
					status.State = "dispatched"
					status.RunID = "local-trigger-run"
				}
				if mode == "rejected" {
					status.State = "rejected"
					status.Reason = "workflow disabled"
				}
				_ = json.NewEncoder(w).Encode(status)
			})
			layout := instance.NewLayout(root)
			writeFileContent(t, filepath.Join(root, instance.RootIdentityFileName), "0123456789abcdef0123456789abcdef\n")
			createTelemetryRetentionRun(t, layout, "local-trigger-run", time.Now())
			release, err := acquireDaemonLock(filepath.Join(layout.SchedulerDir(), "up.lock"), root, time.Minute, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			args := []string{"run", "--request-id", "delivery", "default-implement", root}
			if mode == "no-wait" {
				args = append(args, "--no-wait")
			}
			code, stdout, stderr := runArgs(t, args...)
			want := 0
			if mode == "rejected" {
				want = 1
			}
			if mode == "status-unavailable" {
				want = 2
			}
			if code != want || posts.Load() != 1 || !strings.Contains(stdout, "accepted trigger trigger-test") {
				t.Fatalf("code=%d posts=%d stdout=%q stderr=%q", code, posts.Load(), stdout, stderr)
			}
			if mode == "no-wait" && gets.Load() != 0 {
				t.Fatal("no-wait polled dispatch")
			}
			if mode == "completed" && !strings.Contains(stdout, "finished: phase=completed") {
				t.Fatalf("did not wait for completion: %q", stdout)
			}
			if mode == "status-unavailable" && !strings.Contains(stderr, "remains accepted") {
				t.Fatalf("lost acceptance: %q", stderr)
			}
			if _, err := os.Stat(filepath.Join(layout.SchedulerDir(), pendingTriggersDir)); !os.IsNotExist(err) {
				t.Fatalf("implicit file delegation: %v", err)
			}
		})
	}
}

func TestRunNoAPIFlagCanFollowWorkflow(t *testing.T) {
	for _, flag := range []string{"--no-api", "--no-api=true", "-no-api", "-no-api=true"} {
		args := runFlagArgs([]string{"workflow", flag, "root"})
		if len(args) != 3 || args[0] != flag || args[1] != "workflow" || args[2] != "root" {
			t.Fatalf("args=%v", args)
		}
	}
}
