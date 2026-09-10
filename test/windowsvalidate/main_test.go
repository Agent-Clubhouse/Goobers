//go:build windows

package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func TestValidateImplementationJournal(t *testing.T) {
	runDir := implementationJournal(t, true, implementationEvents())
	if err := validateImplementationJournal(runDir); err != nil {
		t.Fatalf("validateImplementationJournal: %v", err)
	}
}

func TestValidateImplementationJournalRejectsIncompleteRun(t *testing.T) {
	runDir := implementationJournal(t, false, implementationEvents())
	err := validateImplementationJournal(runDir)
	if err == nil || !strings.Contains(err.Error(), `phase = "running", want "completed"`) {
		t.Fatalf("validateImplementationJournal error = %v, want running phase rejection", err)
	}
}

func TestValidateImplementationJournalRejectsWrongSequence(t *testing.T) {
	events := implementationEvents()
	events = events[:len(events)-1]
	runDir := implementationJournal(t, true, events)
	err := validateImplementationJournal(runDir)
	if err == nil || !strings.Contains(err.Error(), "implementation workflow sequence") {
		t.Fatalf("validateImplementationJournal error = %v, want sequence rejection", err)
	}
}

func TestConfigureEphemeralAPI(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "instance.yaml")
	if err := instance.WriteConfig(path, &instance.Config{
		APIVersion: instance.ConfigAPIVersion,
		Kind:       instance.ConfigKind,
	}); err != nil {
		t.Fatal(err)
	}

	if err := configureEphemeralAPI(root); err != nil {
		t.Fatal(err)
	}
	config, err := instance.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.API.Listen != ephemeralAPIListenAddress {
		t.Fatalf("API listen = %q, want %q", config.API.Listen, ephemeralAPIListenAddress)
	}
}

func TestWaitForDaemonReadinessWaitsForReadyProbe(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != httpapi.ReadinessPath {
			t.Errorf("path = %q, want %q", request.URL.Path, httpapi.ReadinessPath)
		}
		ready := requests.Add(1) >= 2
		status := http.StatusServiceUnavailable
		if ready {
			status = http.StatusOK
		}
		response.WriteHeader(status)
		if _, err := fmt.Fprintf(response, `{"ready":%t}`, ready); err != nil {
			t.Errorf("write readiness response: %v", err)
		}
	}))
	defer server.Close()

	root := t.TempDir()
	addressPath := filepath.Join(instance.NewLayout(root).SchedulerDir(), daemonAPIAddressFileName)
	if err := os.MkdirAll(filepath.Dir(addressPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(addressPath, []byte(strings.TrimPrefix(server.URL, "http://")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	address, exited, err := waitForDaemonReadiness(root, make(chan error), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if exited {
		t.Fatal("process reported exited")
	}
	if address != strings.TrimPrefix(server.URL, "http://") {
		t.Fatalf("address = %q, want %q", address, strings.TrimPrefix(server.URL, "http://"))
	}
	if requests.Load() < 2 {
		t.Fatalf("readiness requests = %d, want at least 2", requests.Load())
	}
}

func TestWaitForDaemonReadinessReportsEarlyExit(t *testing.T) {
	waitErr := make(chan error, 1)
	waitErr <- errors.New("startup failed")

	_, exited, err := waitForDaemonReadiness(t.TempDir(), waitErr, time.Second)
	if !exited {
		t.Fatal("process exit was not reported")
	}
	if err == nil || !strings.Contains(err.Error(), "startup failed") {
		t.Fatalf("error = %v, want startup failure", err)
	}
}

func TestFirstLine(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "first\r\nsecond\r\n", want: "first"},
		{input: "  only  ", want: "only"},
		{input: "", want: ""},
	}
	for _, test := range tests {
		if got := firstLine(test.input); got != test.want {
			t.Errorf("firstLine(%q) = %q, want %q", test.input, got, test.want)
		}
	}
}

func implementationJournal(t *testing.T, completed bool, events []journal.Event) string {
	t.Helper()
	runsDir := t.TempDir()
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID:           "0af7651916cd43dd8448eb211c80319c",
		Workflow:        "implementation",
		WorkflowVersion: 1,
		Gaggle:          "goobers",
		Trigger:         journal.Trigger{Kind: journal.TriggerItem, Ref: "issue-2031"},
	}, nil)
	if err != nil {
		t.Fatalf("create journal: %v", err)
	}
	for _, event := range events {
		if err := run.Append(event); err != nil {
			t.Fatalf("append %s: %v", event.Type, err)
		}
	}
	if completed {
		if err := run.Append(journal.Event{
			Type:   journal.EventRunFinished,
			Status: string(journal.PhaseCompleted),
		}); err != nil {
			t.Fatalf("finish journal: %v", err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
	return filepath.Join(runsDir, "0af7651916cd43dd8448eb211c80319c")
}

func implementationEvents() []journal.Event {
	return []journal.Event{
		{Type: journal.EventStageStarted, Stage: "query-backlog"},
		{Type: journal.EventStageStarted, Stage: "gather-implement-context"},
		{Type: journal.EventStageStarted, Stage: "warm-module-cache"},
		{Type: journal.EventGateEvaluated, Gate: "warm-module-cache-gate", Verdict: "pass"},
		{Type: journal.EventStageStarted, Stage: "implement"},
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "pass"},
		{Type: journal.EventStageStarted, Stage: "push-branch"},
		{Type: journal.EventStageStarted, Stage: "local-ci"},
		{Type: journal.EventGateEvaluated, Gate: "local-gate", Verdict: "pass"},
		{Type: journal.EventStageStarted, Stage: "open-pr"},
		{Type: journal.EventGateEvaluated, Gate: "open-pr-gate", Verdict: "pass"},
		{Type: journal.EventStageStarted, Stage: "ci-poll"},
		{Type: journal.EventGateEvaluated, Gate: "ci-gate", Verdict: "pass"},
		{Type: journal.EventStageStarted, Stage: "close-out"},
	}
}
