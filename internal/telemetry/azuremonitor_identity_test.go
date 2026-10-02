package telemetry

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"testing"

	"github.com/microsoft/ApplicationInsights-Go/appinsights"
	"github.com/microsoft/ApplicationInsights-Go/appinsights/contracts"
)

func TestAzureMonitorHostIdentitySurvivesProcessTag(t *testing.T) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Fatalf("hostname unavailable: %v", err)
	}
	for _, consent := range []bool{false, true} {
		t.Run(strconv.FormatBool(consent), func(t *testing.T) {
			payloads, server := azureMonitorTestServer(t)
			defer server.Close()
			client, err := newAzureMonitorClient("InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint="+server.URL, server.Client(), consent, azureReplayConfig{})
			if err != nil {
				t.Fatal(err)
			}
			item := appinsights.NewTraceTelemetry("synthetic identity observation", contracts.Information)
			item.Tags.Cloud().SetRoleInstance("opaque-process-instance")
			if err := client.export(context.Background(), []appinsights.Telemetry{item}); err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				Tags map[string]string
				Data struct {
					BaseData struct{ Properties map[string]string }
				}
			}
			if err := json.Unmarshal([]byte(waitAzureMonitorPayload(t, payloads)), &envelope); err != nil {
				t.Fatal(err)
			}
			want := ""
			if consent {
				want = host
			}
			if got := envelope.Data.BaseData.Properties["host.name"]; got != want {
				t.Fatalf("host.name=%q, want %q", got, want)
			}
			if envelope.Tags[contracts.CloudRoleInstance] != "opaque-process-instance" {
				t.Fatal("machine identity replaced the process correlation tag")
			}
			if !consent && envelope.Tags[contracts.DeviceId] != "" {
				t.Fatal("hostname tag escaped without diagnostic consent")
			}
		})
	}
}
