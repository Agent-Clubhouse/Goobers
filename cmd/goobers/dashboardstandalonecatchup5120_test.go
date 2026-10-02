package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

// isolateStandaloneReadModelCache points os.UserCacheDir at a test directory so
// the standalone read-model cache is created fresh and never touches the
// operator's real cache.
func isolateStandaloneReadModelCache(t *testing.T) {
	t.Helper()
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)
	t.Setenv("LocalAppData", cache)
}

// createStandaloneTestRun writes a run journal the way a separate
// `goobers run --no-api` process would. An unfinished run is returned open so
// the test can finish it later; a finished one is closed and nil is returned.
func createStandaloneTestRun(t *testing.T, layout instance.Layout, runID string, finished bool) *journal.Run {
	t.Helper()
	run, err := journal.Create(layout.ForGaggle("example").RunsDir(), journal.RunIdentity{
		RunID:           runID,
		Workflow:        "implementation",
		WorkflowVersion: 1,
		Gaggle:          "example",
		StartedAt:       time.Now(),
		Trigger:         journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !finished {
		return run
	}
	finishStandaloneTestRun(t, run)
	return nil
}

func finishStandaloneTestRun(t *testing.T, run *journal.Run) {
	t.Helper()
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
		_ = run.Close()
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
}

func standaloneRunPhases(t *testing.T, api dashboardAPI) map[string]journal.RunPhase {
	t.Helper()
	response := httptest.NewRecorder()
	api.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, httpapi.RunsPath, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("%s status = %d, body = %q", httpapi.RunsPath, response.Code, response.Body.String())
	}
	var list readservice.RunList
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	phases := make(map[string]journal.RunPhase, len(list.Runs))
	for _, run := range list.Runs {
		phases[run.ID] = run.Phase
	}
	return phases
}

// waitForStandaloneRunPhases polls the run list until every wanted run reports
// its wanted phase. Convergence is asynchronous by design (a rate-bounded
// sweep), so the deadline is generous and only bounds a hang.
func waitForStandaloneRunPhases(t *testing.T, api dashboardAPI, want map[string]journal.RunPhase) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		got := standaloneRunPhases(t, api)
		matched := true
		for runID, phase := range want {
			if got[runID] != phase {
				matched = false
			}
		}
		if matched {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("standalone run list = %v, want %v", got, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestStandaloneDashboardDiscoversRunsAfterCacheIsPopulated is #5120's repro: a
// standalone dashboard whose cache is already populated must list runs a
// separate CLI process creates afterwards, see a cached `running` row reach its
// terminal phase, and catch up a stale cache on restart.
func TestStandaloneDashboardDiscoversRunsAfterCacheIsPopulated(t *testing.T) {
	root := initDemo(t)
	isolateStandaloneReadModelCache(t)
	layout := instance.NewLayout(root)
	config, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	const (
		existingRun = "11111111111111111111111111111111"
		runningRun  = "22222222222222222222222222222222"
		laterRun    = "33333333333333333333333333333333"
		offlineRun  = "44444444444444444444444444444444"
	)
	createStandaloneTestRun(t, layout, existingRun, true)
	running := createStandaloneTestRun(t, layout, runningRun, false)

	api, err := standaloneDashboardAPI(layout, config, log.New(io.Discard, "", 0), true)
	if err != nil {
		_ = running.Close()
		t.Fatal(err)
	}
	waitForStandaloneRunPhases(t, api, map[string]journal.RunPhase{
		existingRun: journal.PhaseCompleted,
		runningRun:  journal.PhaseRunning,
	})

	// A new CLI run completes while the dashboard serves the populated cache,
	// and the run the cache recorded as running finishes.
	createStandaloneTestRun(t, layout, laterRun, true)
	finishStandaloneTestRun(t, running)
	waitForStandaloneRunPhases(t, api, map[string]journal.RunPhase{
		existingRun: journal.PhaseCompleted,
		runningRun:  journal.PhaseCompleted,
		laterRun:    journal.PhaseCompleted,
	})
	if err := api.close(); err != nil {
		t.Fatal(err)
	}

	// A run created while no dashboard is serving is found after a restart
	// that keeps the populated cache.
	createStandaloneTestRun(t, layout, offlineRun, true)
	before := snapshotDashboardInstance(t, root)
	restarted, err := standaloneDashboardAPI(layout, config, log.New(io.Discard, "", 0), true)
	if err != nil {
		t.Fatal(err)
	}
	waitForStandaloneRunPhases(t, restarted, map[string]journal.RunPhase{
		existingRun: journal.PhaseCompleted,
		runningRun:  journal.PhaseCompleted,
		laterRun:    journal.PhaseCompleted,
		offlineRun:  journal.PhaseCompleted,
	})
	if err := restarted.close(); err != nil {
		t.Fatal(err)
	}
	// The sweep that found offlineRun only read the instance: catching up the
	// derived cache must leave the authoritative instance byte-identical.
	if after := snapshotDashboardInstance(t, root); !reflect.DeepEqual(after, before) {
		t.Fatalf("standalone catch-up changed instance files\nbefore: %#v\nafter:  %#v", before, after)
	}
}
