package main

import (
	"context"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

type recordingDivergenceAppender struct {
	events chan journal.Event
}

func TestWorkerDivergenceJournalRecorderDeduplicatesAcrossRestart(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	recorder, err := newWorkerDivergenceJournalRecorder(log)
	if err != nil {
		t.Fatal(err)
	}
	event := journal.Event{Type: journal.EventWorkerConfigDivergence, Runner: map[string]any{
		"worker": "worker:a", "state": workerDivergenceInSync,
		"workerDigest": "sha256:a", "daemonDigest": "sha256:a", "message": "in sync",
	}}
	if err := recorder.Append(event); err != nil {
		t.Fatal(err)
	}
	restarted, err := newWorkerDivergenceJournalRecorder(log)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Append(event); err != nil {
		t.Fatal(err)
	}
	event.Runner["state"] = workerDivergenceDiverged
	event.Runner["daemonDigest"] = "sha256:b"
	event.Runner["message"] = "diverged"
	if err := restarted.Append(event); err != nil {
		t.Fatal(err)
	}
	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Runner["state"] != workerDivergenceInSync || events[1].Runner["state"] != workerDivergenceDiverged {
		t.Fatalf("events = %+v", events)
	}
}

func TestWorkerDivergenceRecorderReopensAwaitingPeriodOnDaemonRestart(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	recorder, err := newWorkerDivergenceJournalRecorder(log)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &instance.Config{Engine: &instance.EngineConfig{HostPort: "temporal:7233"}}
	cfg.API.PodTokenKeyFile = "/var/run/goobers/pod.key"
	if err := recordDaemonWorkerDivergenceAvailability(recorder, cfg); err != nil {
		t.Fatal(err)
	}
	workerReport := journal.Event{Type: journal.EventWorkerConfigDivergence, Runner: map[string]any{
		"worker": "worker:node-a", "state": workerDivergenceInSync,
		"workerDigest": "sha256:a", "daemonDigest": "sha256:a", "message": "reported",
	}}
	if err := recorder.Append(workerReport); err != nil {
		t.Fatal(err)
	}

	restarted, err := newWorkerDivergenceJournalRecorder(log)
	if err != nil {
		t.Fatal(err)
	}
	if err := recordDaemonWorkerDivergenceAvailability(restarted, cfg); err != nil {
		t.Fatal(err)
	}
	// The worker's first report in this daemon lifetime must be durable even
	// though it is byte-for-byte identical to the previous lifetime's report.
	if err := restarted.Append(workerReport); err != nil {
		t.Fatal(err)
	}
	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 || events[2].Runner["worker"] != journal.WorkerConfigDivergenceReportingCapability ||
		events[3].Runner["worker"] != "worker:node-a" {
		t.Fatalf("restart events = %+v, want fresh sentinel followed by durable worker resolution", events)
	}
	service, err := readservice.NewLocal(readservice.LocalSources{
		Layout: layout, Definitions: &instance.ConfigSet{Manifest: &apiv1.Manifest{}},
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	status, err := service.SchedulerStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.WorkerConfigDivergence) != 1 || status.WorkerConfigDivergence[0].Worker != "worker:node-a" {
		t.Fatalf("post-restart status retained sentinel: %+v", status.WorkerConfigDivergence)
	}
}

func TestDaemonRecordsRemoteReportingNotActiveWithoutWorkerKey(t *testing.T) {
	events := make(chan journal.Event, 1)
	cfg := &instance.Config{Engine: &instance.EngineConfig{HostPort: "temporal:7233"}}
	if err := recordDaemonWorkerDivergenceAvailability(&recordingDivergenceAppender{events: events}, cfg); err != nil {
		t.Fatal(err)
	}
	assertDivergenceEvent(t, events, journal.WorkerConfigDivergenceReportingCapability, workerDivergenceNotActive)
	cfg.API.PodTokenKeyFile = "/var/run/goobers/pod.key"
	if err := recordDaemonWorkerDivergenceAvailability(&recordingDivergenceAppender{events: events}, cfg); err != nil {
		t.Fatal(err)
	}
	assertDivergenceEvent(t, events, journal.WorkerConfigDivergenceReportingCapability, workerDivergenceNotChecked)
}

func assertDivergenceEvent(t *testing.T, events <-chan journal.Event, worker, state string) {
	t.Helper()
	select {
	case event := <-events:
		if event.Type != journal.EventWorkerConfigDivergence || event.Runner["worker"] != worker || event.Runner["state"] != state {
			t.Fatalf("divergence event = %+v", event)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("missing %s divergence event", state)
	}
}

func (r *recordingDivergenceAppender) Append(event journal.Event) error {
	if r.events != nil {
		r.events <- event
	}
	return nil
}
