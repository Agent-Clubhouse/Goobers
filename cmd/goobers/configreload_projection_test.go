package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

func TestConfigReloadPublishesHotAddedGaggleRuns(t *testing.T) {
	previousInterval := configReloadInterval
	configReloadInterval = 20 * time.Millisecond
	t.Cleanup(func() { configReloadInterval = previousInterval })

	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	workflow := func(gaggle string) []byte {
		return fmt.Appendf(nil, `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: default-implement
spec:
  gaggle: %s
  triggers:
    - type: manual
  start: work
  tasks:
    - name: work
      type: deterministic
      goal: Complete a local task before approval.
      run:
        command: [%q, "-test.run=^$"]
        workspace: scratch
      next: approval
    - name: finish
      type: deterministic
      goal: Complete the approved run.
      run:
        command: [%q, "-test.run=^$"]
        workspace: scratch
  gates:
    - name: approval
      evaluator: human
      human: {}
      branches:
        pass: finish
        fail: "@abort"
`, gaggle, testBinary, testBinary)
	}
	daemon := startUpOnFreeLoopback(t, freeLoopbackAddress, func(address string) string {
		root := initDemo(t)
		layout := instance.NewLayout(root)
		setAPIListenAddress(t, root, address)
		allowParallelFixtureRuns(t, layout, 2)
		path := filepath.Join(layout.ConfigDir(), "gaggles", "example", "workflows", "default-implement.yaml")
		if err := os.WriteFile(path, workflow("example"), 0o644); err != nil {
			t.Fatal(err)
		}
		return root
	})
	t.Cleanup(func() {
		daemon.cancel()
		select {
		case code := <-daemon.done:
			if code != 0 {
				t.Errorf("daemon exit code = %d, stderr = %s", code, daemon.stderr.String())
			}
		case <-time.After(10 * time.Second):
			t.Error("daemon did not stop")
		}
	})

	layout := instance.NewLayout(daemon.root)
	client := &http.Client{Timeout: 2 * time.Second}
	get := func(path string, out any) int {
		t.Helper()
		response, err := client.Get("http://" + daemon.address + path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode == http.StatusOK {
			if err := json.NewDecoder(response.Body).Decode(out); err != nil {
				t.Fatal(err)
			}
		} else if response.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s: status %d", path, response.StatusCode)
		}
		return response.StatusCode
	}
	startRun := func(gaggle string) string {
		t.Helper()
		code, stdout, stderr := runArgs(t, "run", "--no-wait", "--gaggle", gaggle, "default-implement", daemon.root)
		if code != 0 {
			t.Fatalf("start %s run: code=%d stdout=%q stderr=%q", gaggle, code, stdout, stderr)
		}
		runID := runIDFromAcceptedTriggerStdout(t, layout, stdout)
		waitForConfigValue(t, gaggle+" run detail to reach approval", func() (readservice.RunDetail, bool) {
			var detail readservice.RunDetail
			status := get(httpapi.RunsPath+"/"+runID, &detail)
			return detail, status == http.StatusOK && detail.Phase == journal.PhaseRunning &&
				detail.CurrentStage == "approval"
		})
		return runID
	}
	waitForListedRun := func(gaggle, runID string, phase journal.RunPhase) {
		t.Helper()
		waitForConfigValue(t, gaggle+" run "+runID+" to appear in the Portal list as "+string(phase), func() (readservice.RunList, bool) {
			var list readservice.RunList
			get(httpapi.RunsPath+"?gaggle="+gaggle+"&limit=50&showNoWork=true", &list)
			for _, run := range list.Runs {
				if run.ID == runID && run.Gaggle == gaggle && run.Phase == phase {
					return list, true
				}
			}
			return list, false
		})
	}

	existingRun := startRun("example")
	waitForListedRun("example", existingRun, journal.PhaseRunning)
	initialHealth := readDaemonHealth(t, daemon.address)

	gaggle, err := os.ReadFile(filepath.Join(layout.ConfigDir(), "gaggles", "example", "gaggle.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	beta := filepath.Join(layout.ConfigDir(), "gaggles", "beta")
	if err := os.MkdirAll(filepath.Join(beta, "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beta, "gaggle.yaml"), []byte(strings.ReplaceAll(string(gaggle), "example", "beta")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beta, "workflows", "default-implement.yaml"), workflow("beta"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(layout.ConfigDir(), "manifest.yaml")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte(strings.Replace(string(manifest), "    - example\n", "    - example\n    - beta\n", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForConfigValue(t, "hot-added beta workflow to be admitted", func() (readservice.Health, bool) {
		health := readDaemonHealth(t, daemon.address)
		return health, health.DefinitionReload != nil && health.DefinitionReload.State == "current" &&
			health.Freshness.DefinitionsLoadedAt.After(initialHealth.Freshness.DefinitionsLoadedAt)
	})

	hotAddedRun := startRun("beta")
	waitForListedRun("beta", hotAddedRun, journal.PhaseRunning)
	for _, run := range []struct{ gaggle, id string }{
		{"example", existingRun}, {"beta", hotAddedRun},
	} {
		code, stdout, stderr := runArgs(t, "approve", "--actor=projection-test", run.id, "approval", daemon.root)
		if code != 0 {
			t.Fatalf("approve %s: code=%d stdout=%q stderr=%q", run.gaggle, code, stdout, stderr)
		}
		waitForListedRun(run.gaggle, run.id, journal.PhaseCompleted)
		var detail readservice.RunDetail
		if get(httpapi.RunsPath+"/"+run.id, &detail) != http.StatusOK || detail.Phase != journal.PhaseCompleted {
			t.Fatalf("%s completed run detail = %+v", run.gaggle, detail)
		}
	}

	beforeRemoval := readDaemonHealth(t, daemon.address)
	if err := os.WriteFile(manifestPath, manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	waitForDefinitionsReload(t, daemon.address, beforeRemoval.Freshness.DefinitionsLoadedAt)
	waitForListedRun("beta", hotAddedRun, journal.PhaseCompleted)
	waitForListedRun("example", existingRun, journal.PhaseCompleted)
}
