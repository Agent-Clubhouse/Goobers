package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/diagnostics/executiondeadline"
	"github.com/goobers/goobers/internal/diagnostics/fleetstate"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readservice"
)

func TestFleetActualExecutionDeadlineDoesNotMaskUnknownSibling(t *testing.T) {
	now := time.Now().UTC()
	deadline := now.Add(time.Hour)
	run := readservice.RunSummary{Gaggle: "g", ActiveStages: []readmodel.ActiveStage{{Name: "quiet", Kind: "stage", ExecutionID: "execution", ExecutionObservedAt: &now, ExecutionDeadline: &deadline}}}
	runs := []readservice.RunSummary{run}
	one := 1
	observation := fleetstate.Observation{ObservedAt: now, Complete: true, InflightCount: &one, EligibleCount: &one, OldestEligibleAt: now.Add(-time.Hour), ActiveDeadline: fleetExecutionDeadline(runs, "g", now)}
	if got := fleetstate.Classify(observation, now, time.Minute, time.Minute); got.State != "waiting" || got.ReasonCode != "stage_within_deadline" {
		t.Fatal(got)
	}
	runs[0].ActiveStages = append(runs[0].ActiveStages, readmodel.ActiveStage{Name: "unobserved", Kind: "stage"})
	observation.ActiveDeadline = fleetExecutionDeadline(runs, "g", now)
	if got := fleetstate.Classify(observation, now, time.Minute, time.Minute); got.State != "unknown" {
		t.Fatal("unbounded sibling masked", got)
	}
	runs = []readservice.RunSummary{run}
	runs[0].ActivityTruncated = true
	if !fleetExecutionDeadline(runs, "g", now).IsZero() {
		t.Fatal("truncated stage inventory claimed coverage")
	}
}

func TestRemoteExecutionDeadlineReachesCurrentActiveStage(t *testing.T) {
	runsDir := t.TempDir()
	run, err := journal.Create(runsDir, journal.RunIdentity{RunID: "deadline-run", Gaggle: "g", Workflow: "w", Driver: journal.DriverEngine}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "stage", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	writer, err := livejournal.NewWriter(func(gaggle string) (string, bool) { return runsDir, gaggle == "g" })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request livejournal.EmitRequest
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			w.WriteHeader(400)
			return
		}
		response, err := writer.Emit(r.Context(), request)
		if err != nil {
			w.WriteHeader(500)
			return
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	t.Setenv(dispatcher.EnvDaemonAPI, server.URL)
	t.Setenv(dispatcher.EnvRunID, "deadline-run")
	t.Setenv(dispatcher.EnvGaggle, "g")
	t.Setenv(dispatcher.EnvStage, "stage")
	t.Setenv(dispatcher.EnvPodToken, "synthetic-token")
	t.Setenv(dispatcher.EnvPodAttempt, "attempt-1")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ctx = executiondeadline.WithRecorder(ctx, podArtifactRecorder{dir: t.TempDir()}, "stage", 1)
	started, release := make(chan struct{}), make(chan struct{})
	waitServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-release; w.WriteHeader(204) }))
	defer waitServer.Close()
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	processJoined := make(chan struct{})
	processDone := make(chan error, 1)
	go func() {
		defer close(processJoined)
		_, err := (harness.ExecProcessRunner{}).Run(ctx, harness.ProcessRequest{Command: []string{os.Args[0], "-test.run=^TestRemoteDeadlineProcessHelper$", "--", waitServer.URL, "remote-deadline-helper"}, Timeout: time.Minute})
		processDone <- err
	}()
	defer func() { unblock(); cancel(); <-processJoined }()
	select {
	case <-started:
	case err := <-processDone:
		t.Fatalf("process ended before observation: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("process never reached live boundary")
	}

	read := func() readmodel.StageActivity {
		reader, err := journal.OpenReadOnly(filepath.Join(runsDir, "deadline-run"))
		if err != nil {
			t.Fatal(err)
		}
		events, err := reader.Events()
		if err != nil {
			t.Fatal(err)
		}
		var activity readmodel.StageActivity
		for _, event := range events {
			activity = activity.After(event)
		}
		return activity
	}
	active := read()
	if len(active.Active) != 1 || active.Active[0].ExecutionDeadline == nil {
		t.Fatalf("remote active deadline absent: %+v", active)
	}
	unblock()
	if err := <-processDone; err != nil {
		t.Fatal(err)
	}
	if ended := read(); len(ended.Active) != 1 || ended.Active[0].ExecutionDeadline != nil {
		t.Fatalf("finished process retained bound: %+v", ended)
	}
}

func TestRemoteDeadlineProcessHelper(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-1] != "remote-deadline-helper" {
		return
	}
	response, err := http.Get(os.Args[len(os.Args)-2])
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
}
