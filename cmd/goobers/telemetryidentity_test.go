package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	collectorlog "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/encoding/prototext"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func TestTelemetryInstanceIdentities(t *testing.T) {
	t.Run("scheduler-first-boot", testTelemetrySchedulerFirstBoot)
	journalID, rootID := strings.Repeat("1", 32), strings.Repeat("2", 32)
	t.Run("explicit-root-required", func(t *testing.T) {
		root := t.TempDir()
		for _, name := range []string{"instance-id", instance.RootIdentityFileName} {
			if err := os.WriteFile(filepath.Join(root, name), []byte(journalID+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		t.Chdir(root)
		for _, absent := range []string{"", " "} {
			if id, attrs := telemetryInstanceIdentities(absent); id != "" || len(attrs) != 0 {
				t.Fatal("missing root adopted ambient working-directory identity")
			}
		}
	})
	for _, tc := range []struct {
		name, journal, root   string
		wantJournal, wantRoot string
	}{
		{"distinct", journalID, rootID, journalID, rootID},
		{"missing", "", "", "", ""},
		{"journal-only", journalID, "", journalID, ""},
		{"root-only", "", rootID, "", rootID},
		{"invalid-journal", "broken", rootID, "", rootID},
		{"invalid-root", journalID, "broken", journalID, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			files := map[string]string{"instance-id": tc.journal, instance.RootIdentityFileName: tc.root}
			count := 0
			for name, value := range files {
				if value != "" {
					count++
					if err := os.WriteFile(filepath.Join(root, name), []byte(value+"\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			id, attrs := telemetryInstanceIdentities(root)
			got := map[string]string{}
			for _, attr := range attrs {
				got[string(attr.Key)] = attr.Value.AsString()
			}
			if id != tc.wantJournal || got["goobers.instance.id"] != tc.wantJournal || got["goobers.root.id"] != tc.wantRoot {
				t.Fatalf("journal=%q resources=%v", id, got)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != count {
				t.Fatalf("identity observation created state: entries=%d error=%v", len(entries), err)
			}
			for name, value := range files {
				if value != "" {
					data, err := os.ReadFile(filepath.Join(root, name))
					if err != nil || string(data) != value+"\n" {
						t.Fatalf("identity observation changed %s", name)
					}
				}
			}
		})
	}
}

func testTelemetrySchedulerFirstBoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "first-boot")
	if code, _, stderr := runArgs(t, "init", "--demo", "--allow-ephemeral", "--insecure", root); code != 0 {
		t.Fatalf("init: %d %s", code, stderr)
	}
	layout := instance.NewLayout(root)
	if _, err := layout.ReadIdentity(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture must not pre-create the runner identity: %v", err)
	}
	collector := &routingCollector{}
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	enabled := true
	cfg.Telemetry.Enabled = &enabled
	cfg.Telemetry.OTLP = &instance.OTLPConfig{Endpoint: startRoutingCollector(t, collector), Insecure: true}
	if err := instance.WriteConfig(layout.ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}
	var firstID string
	for boot := range 2 {
		var wg sync.WaitGroup
		setup, err := buildSchedulerSetup(t.Context(), layout, &wg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = setup.Shutdown(context.Background()) })
		id, err := layout.ReadIdentity()
		if err != nil {
			t.Fatal(err)
		}
		if boot == 0 {
			firstID = id
		} else if id != firstID {
			t.Fatal("restart rotated the durable identity")
		}
		if err := setup.InstanceLog.Append(journal.Event{Type: journal.EventRunnerAnnotation, Reason: "first-boot identity probe"}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = setup.Shutdown(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		assertSchedulerTelemetryIdentity(t, collector, id)
	}
}

func assertSchedulerTelemetryIdentity(t *testing.T, collector *routingCollector, id string) {
	t.Helper()
	collector.mu.Lock()
	defer collector.mu.Unlock()
	found := false
	for _, observation := range collector.observations {
		if observation.signal != "logs" {
			continue
		}
		request := &collectorlog.ExportLogsServiceRequest{}
		if err := prototext.Unmarshal([]byte(observation.payload), request); err != nil {
			t.Fatal(err)
		}
		for _, resource := range request.ResourceLogs {
			resourceID := ""
			for _, attr := range resource.Resource.Attributes {
				if attr.Key == "goobers.instance.id" {
					resourceID = attr.Value.GetStringValue()
				}
			}
			for _, scope := range resource.ScopeLogs {
				for _, record := range scope.LogRecords {
					if !strings.Contains(record.Body.GetStringValue(), "first-boot identity probe") {
						continue
					}
					found = true
					recordID := ""
					for _, attr := range record.Attributes {
						if attr.Key == "goobers.instance.id" {
							recordID = attr.Value.GetStringValue()
						}
					}
					if resourceID != id || recordID != id {
						t.Fatalf("first-boot scheduler identity: resource=%q record=%q want=%q", resourceID, recordID, id)
					}
				}
			}
		}
	}
	if !found {
		t.Fatal("scheduler probe was not exported")
	}
	collector.observations = nil
}
