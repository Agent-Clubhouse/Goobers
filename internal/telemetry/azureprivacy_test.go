package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAzureMonitorHostIdentityLegacyJournalReplayProjection(t *testing.T) {
	for _, consent := range []bool{false, true} {
		t.Run(strconv.FormatBool(consent), func(t *testing.T) {
			payloads, server := azureMonitorTestServer(t)
			defer server.Close()
			root := t.TempDir()
			dir := filepath.Join(root, "journal")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			const body = `{"schema":"goobers.dev/journal/event/v1","type":"service.health","seq":48,"runner":{"machineName":"private-host","accountName":"private-account","instanceId":"durable-instance","recoveryInventory":{"state":"healthy","inventoryRoot":"private-path"}}}`
			stableID := strings.Repeat("a", 64)
			legacy, err := json.Marshal(map[string]any{"name": "Microsoft.ApplicationInsights.Message", "iKey": "00000000-0000-0000-0000-000000000000", "data": map[string]any{"baseType": "MessageData", "baseData": map[string]any{"ver": 2, "message": body, "properties": map[string]string{"goobers.journal.kind": "scheduler", "goobers.journal.seq": "48", "goobers.telemetry.record_id": stableID}}}})
			if err != nil {
				t.Fatal(err)
			}
			header, err := json.Marshal(azureReplayHeader{Schema: azureReplaySchema, CreatedAt: time.Now().Add(-time.Minute), Records: 1})
			if err != nil {
				t.Fatal(err)
			}
			// An old, already-encoded spool file bypasses current journal/SDK
			// serialization. The final HTTP boundary must still protect it.
			file := append(append(append(header, '\n'), legacy...), '\n')
			if err := os.WriteFile(filepath.Join(dir, "00000000000000000001-legacy.ndjson"), file, 0o600); err != nil {
				t.Fatal(err)
			}
			client, err := newAzureMonitorClient("InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint="+server.URL,
				server.Client(), consent, azureReplayConfig{root: root, dir: dir, maxAge: time.Hour, maxBytes: 1 << 20})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := client.replay.close(ctx); err != nil {
					t.Error(err)
				}
			}()
			got := waitAzureMonitorPayload(t, payloads)
			if strings.Contains(got, "private-") || azureRecordID(got) != stableID || !strings.Contains(got, "durable-instance") {
				t.Fatalf("legacy replay leaked identity or lost stable correlation: %s", got)
			}
			if !bytes.Contains(legacy, []byte("private-host")) {
				t.Fatal("wire projection changed the source buffer")
			}
		})
	}
}

func TestAzureMonitorHostIdentityMixedPayloadProjection(t *testing.T) {
	const ordinary = `{"data":{"baseData":{"message":"goobers.service.health","properties":{"machineName":"consented-diagnostic-host"}}}}`
	const nonMessage = `{"data":{"baseData":{"measurements":{"count":1}}}}`
	raw := []byte(ordinary + "\n" + nonMessage + "\n")
	got, err := azureJournalHealthPayload(raw)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("identity-consented diagnostic/non-message payload changed: %s, %v", got, err)
	}
	if _, err := azureJournalHealthPayload([]byte(`{"service.health":"private-host"`)); err == nil {
		t.Fatal("malformed candidate bypassed the fail-closed HTTP boundary")
	}
	client := &azureMonitorClient{}
	err = client.sendPayload(t.Context(), []byte(`{"service.health":"private-host"`))
	var remote *azureMonitorDeliveryError
	if err == nil || errors.As(err, &remote) {
		t.Fatalf("local projection failure was masked as remote delivery: %v", err)
	}
}
