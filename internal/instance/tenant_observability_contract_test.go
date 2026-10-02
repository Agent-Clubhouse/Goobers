package instance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTenantObservabilityContractReferenceIsCurrent(t *testing.T) {
	contract := TenantTelemetryContract()
	if contract.Schema != TenantObservabilityContractVersion || len(contract.Incidents) == 0 {
		t.Fatalf("incomplete tenant observability contract: %+v", contract)
	}
	for _, incident := range contract.Incidents {
		if incident.ID == "" || incident.Signal == "" || len(incident.EventNames) == 0 ||
			len(incident.StableCodes) == 0 || len(incident.RequiredFields) == 0 || !incident.MinimumProfile.valid() {
			t.Errorf("incomplete incident contract: %+v", incident)
		}
	}
	for _, field := range contract.ForbiddenFields {
		if strings.TrimSpace(field) == "" {
			t.Error("forbidden field names must not be empty")
		}
	}

	generated, err := json.MarshalIndent(contract, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	generated = append(generated, '\n')
	path := filepath.Join("..", "..", "docs", "reference", "tenant-observability-v1.json")
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(generated) != string(want) {
		t.Fatalf("%s is stale; regenerate it from TenantTelemetryContract", path)
	}
}
