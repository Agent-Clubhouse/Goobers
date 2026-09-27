package instance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTelemetryCollectionProfiles(t *testing.T) {
	for _, tc := range []struct {
		profile                   TelemetryCollectionProfile
		journal, traces, identity bool
	}{
		{TelemetryProfileHealth, false, false, false},
		{TelemetryProfileJournal, true, false, false},
		{TelemetryProfileStandard, true, true, false},
		{TelemetryProfileDiagnostic, true, true, true},
	} {
		if !tc.profile.valid() || tc.profile.IncludesJournal() != tc.journal ||
			tc.profile.IncludesTraces() != tc.traces || tc.profile.IncludesHostIdentity() != tc.identity {
			t.Errorf("profile %q contract drifted", tc.profile)
		}
	}
	if got := (TelemetryConfig{}).EffectiveCollectionProfile(); got != TelemetryProfileStandard {
		t.Fatalf("empty profile = %q, want standard", got)
	}
	if err := (&Config{Telemetry: TelemetryConfig{CollectionProfile: "everything"}}).Validate(); err == nil || !strings.Contains(err.Error(), "telemetry.collectionProfile") {
		t.Fatalf("unknown profile error = %v", err)
	}
}

func TestTelemetryCollectionProfileReferenceIsCurrent(t *testing.T) {
	generated, err := json.MarshalIndent(TelemetryCollectionContract(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	generated = append(generated, '\n')
	path := filepath.Join("..", "..", "docs", "reference", "telemetry-collection-profiles-v1.json")
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(generated) != string(want) {
		t.Fatalf("%s is stale; regenerate it from TelemetryCollectionContract", path)
	}
}
