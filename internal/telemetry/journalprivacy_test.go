package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestJournalLogsServiceHealthBodyDoesNotBypassIdentityConsent(t *testing.T) {
	exporter := &journalTestExporter{}
	client := journalTestClient(t, exporter)
	body := []byte(`{"schema":"goobers.dev/journal/event/v1","seq":48,"type":"service.health","branch":0,"time":"2026-09-28T09:50:36Z","runner":{"schemaVersion":2,"machineName":"private-host-fixture","accountName":"private-account-fixture","instanceId":"durable-instance","windowCoverage":"complete","observedUncleanRestarts":9007199254740993,"rawConfig":"private-config-fixture","recoveryInventory":{"state":"healthy","used":0,"limit":128,"inventoryRoot":"private-path-fixture","error":"private-error-fixture"}}}`)
	original := bytes.Clone(body)
	client.Commit(journal.CommittedEvent{Kind: "scheduler", JournalID: "scheduler-id", InstanceID: "durable-instance", Seq: 48, Body: body})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := client.journalLogs.flush(ctx); err != nil {
		t.Fatal(err)
	}
	exporter.mu.Lock()
	defer exporter.mu.Unlock()
	if len(exporter.records) != 1 {
		t.Fatalf("records=%d, want 1", len(exporter.records))
	}
	got := exporter.records[0].Body().AsString()
	for _, private := range []string{"private-host-fixture", "private-account-fixture", "private-config-fixture", "private-path-fixture", "private-error-fixture"} {
		if bytes.Contains([]byte(got), []byte(private)) {
			t.Errorf("journal service-health body exported private field %q", private)
		}
	}
	var projected struct {
		Seq    int
		Type   string
		Runner struct {
			InstanceID              string
			ObservedUncleanRestarts json.Number
			RecoveryInventory       struct {
				State       string
				Used, Limit int
			}
		}
	}
	if err := json.Unmarshal([]byte(got), &projected); err != nil {
		t.Fatal(err)
	}
	if projected.Seq != 48 || projected.Type != "service.health" || projected.Runner.InstanceID != "durable-instance" || projected.Runner.ObservedUncleanRestarts.String() != "9007199254740993" || projected.Runner.RecoveryInventory.State != "healthy" || projected.Runner.RecoveryInventory.Limit != 128 {
		t.Fatalf("projection lost operational data or numeric precision: %s", got)
	}
	if !bytes.Equal(body, original) {
		t.Fatal("export projection changed authoritative caller bytes")
	}
	if azureMonitorLog(&exporter.records[0]).Properties["goobers.telemetry.record_id"] == "" {
		t.Fatal("projection lost stable journal correlation")
	}
}

func TestJournalLogsServiceHealthProjectionEdges(t *testing.T) {
	for _, test := range []struct {
		name, body string
		unchanged  bool
	}{
		{"ordinary", ` {"type":"run.started","runner":{"note":"service.health","n":9007199254740993}} `, true},
		{"diagnostic-name", `goobers.service.health`, true},
		{"escaped-type-and-key", `{"type":"service\u002ehealth","runner":{"\u006dachineName":"private-host","accountName":"private-account","instanceId":"allowed"}}`, false},
		{"nested-scalar", `{"type":"service.health","runner":{"instanceId":{"private":"private-payload"},"observedUncleanRestarts":["private-payload"],"recoveryInventory":{"used":{"private":"private-payload"}}}}`, false},
		{"malformed-envelope", `{"type":"service.health","runner":{"accountName":"private-account"}`, false},
		{"malformed-runner", `{"type":"service.health","runner":["private-account"]}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := []byte(test.body)
			got := journalExportBody(original)
			if string(original) != test.body {
				t.Fatal("projection modified source bytes")
			}
			if test.unchanged {
				if string(got) != test.body {
					t.Fatalf("ordinary body changed: %s", got)
				}
				return
			}
			if !json.Valid(got) || bytes.Contains(got, []byte("private-")) {
				t.Fatalf("unsafe projection: %s", got)
			}
		})
	}
}

func BenchmarkJournalLogsHealthProjection(b *testing.B) {
	for _, size := range []int{1024, 32 << 10} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			body := []byte(`{"type":"run.started","reason":"` + strings.Repeat("x", size) + `"}`)
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for b.Loop() {
				if got := journalExportBody(body); len(got) != len(body) {
					b.Fatal("ordinary body changed")
				}
			}
		})
	}
}
