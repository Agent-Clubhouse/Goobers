package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/test/testsupport/netns"
)

func TestListUsesPublicQueryPaginationAndRejectsIncompleteEvidence(t *testing.T) {
	for _, mode := range []string{"pages", "partial", "broken", "cycle"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				q := r.URL.Query()
				if r.URL.Path != apicontract.RunsPath || q.Get("workflow") != "soak" || q.Get("since") == "" || q.Get("until") == "" || q.Get("phase") != "completed" {
					t.Errorf("query: %v", r.URL)
				}
				page := readservice.RunList{Runs: []readservice.RunSummary{{ID: fmt.Sprint(calls)}}}
				switch mode {
				case "pages":
					if calls == 1 {
						page.NextCursor = "next"
					} else if q.Get("cursor") != "next" {
						t.Errorf("lost cursor")
					}
				case "partial":
					page.ReadState = &readmodel.ReadState{Completeness: readmodel.CompletenessPartial}
				case "cycle":
					page.NextCursor = "same"
				case "broken":
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				if err := json.NewEncoder(w).Encode(page); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			b := cliBackend{endpoint: server.URL, client: server.Client()}
			o := listOptions(time.Now())
			o.Phase = journal.PhaseCompleted
			runs, err := b.List(context.Background(), o)
			if mode == "pages" {
				if err != nil || len(runs) != 2 {
					t.Fatalf("%+v %v", runs, err)
				}
			} else if err == nil {
				t.Fatal("incomplete observation accepted")
			}
		})
	}
}

func TestIsolationLimitsAndCLIRefusal(t *testing.T) {
	p := Presets["smoke"]
	for _, tc := range []struct {
		cpu, memory string
		valid       bool
	}{
		{"200000 100000", "1073741824", true},
		{"max 100000", "1073741824", false},
		{"200000 100000", "max", false},
		{"400000 100000", "1073741824", false},
		{"200000 0", "1073741824", false},
		{"200000 100000", "2147483648", false},
	} {
		if err := checkLimits(p, tc.cpu, tc.memory); (err == nil) != tc.valid {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
	var out, stderr bytes.Buffer
	if code := runMain(context.Background(), []string{"--profile", "typo", "--root", filepath.Join(t.TempDir(), "unused")}, &out, &stderr); code != 2 || out.Len() != 0 {
		t.Fatal("unknown profile reached runtime")
	}
	if runtime.GOOS == "linux" {
		return
	}
	if code := runMain(context.Background(), []string{"--profile", "smoke", "--root", filepath.Join(t.TempDir(), "unused")}, &out, &stderr); code != 2 {
		t.Fatalf("native host exit %d", code)
	}
	var r result
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if r.InvalidReason != containerLaunchFailed || r.Signals.Throughput != nil {
		t.Fatalf("unsafe or measured native-host result: %+v", r)
	}
}

func TestCleanEnvRemovesAmbientDaemonAndProviderRouting(t *testing.T) {
	t.Setenv("GOOBERS_DAEMON_API", "http://wrong.example")
	t.Setenv("GITHUB_TOKEN", "never-used")
	t.Setenv("GH_TOKEN", "never-used")
	for _, e := range cleanEnv() {
		if strings.HasPrefix(e, "GOOBERS_") || strings.HasPrefix(e, "GITHUB_") || strings.HasPrefix(e, "GH_TOKEN=") {
			t.Fatal("ambient authority retained")
		}
	}
}

// This opt-in smoke exercises actual CLI init/up/run and readservice projection.
// It never starts pressure and does not produce a soak verdict. The binary is
// built separately so ordinary package tests stay bounded and deterministic.
func TestRealCLIBackendWithoutPressure(t *testing.T) {
	binary := os.Getenv("SOAK_TEST_GOOBERS")
	if binary == "" {
		t.Skip("set SOAK_TEST_GOOBERS to an already-built CLI")
	}
	netns.RequireIsolation(context.Background(), t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	root := filepath.Join(t.TempDir(), "instance")
	if err := initialize(ctx, binary, root, Presets["smoke"]); err != nil {
		t.Fatal(err)
	}
	b := &cliBackend{binary: binary, root: root, client: &http.Client{Timeout: 3 * time.Second}}
	daemon, err := startChild(ctx, filepath.Join(root, "daemon.log"), binary, "up", "--quiet", root)
	if err != nil {
		t.Fatal(err)
	}
	b.daemon = daemon
	defer daemon.stop()
	t.Cleanup(func() {
		if t.Failed() {
			data, _ := os.ReadFile(filepath.Join(root, "daemon.log"))
			t.Log(string(data))
		}
	})
	if err := b.awaitDaemon(ctx); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, failure := range []bool{false, true} {
		id, err := b.Submit(ctx, failure)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	first := observeRealFixture(t, ctx, b, ids[0], false)
	second := observeRealFixture(t, ctx, b, ids[1], true)
	if !second.StartedAt.Before(*first.FinishedAt) {
		t.Fatal("fixture runs did not overlap")
	}
}

func observeRealFixture(t *testing.T, ctx context.Context, b *cliBackend, id string, failure bool) readservice.RunSummary {
	t.Helper()
	for {
		status, err := b.Resolve(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if status.State == "rejected" {
			t.Fatalf("rejected: %+v", status)
		}
		o := listOptions(time.Now())
		if failure {
			o.Workflow = "soak-failure"
		}
		runs, err := b.List(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range runs {
			if r.ID != status.RunID || !r.Terminal {
				continue
			}
			if failure {
				if r.Phase != journal.PhaseFailed || r.TerminalReason != fixtureFailureReason {
					t.Fatalf("unexpected failure: %+v", r)
				}
			} else if r.Phase != journal.PhaseCompleted {
				t.Fatalf("fixture did not complete: %+v", r)
			}
			if r.FinishedAt == nil {
				t.Fatal("missing completion time")
			}
			return r
		}
		if err := (wallClock{}).Wait(ctx, 100*time.Millisecond); err != nil {
			t.Fatal(err)
		}
	}
}
