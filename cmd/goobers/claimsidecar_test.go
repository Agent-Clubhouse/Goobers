package main

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/providers"
)

func TestClaimSidecarRetainsOutcomeForPodSurrender(t *testing.T) {
	local, remote := reflect.TypeFor[mutationFact](), reflect.TypeFor[dispatcher.SurrenderedMutation]()
	if local.NumField() != remote.NumField() {
		t.Fatal("mutation wire field count drift")
	}
	for i := range local.NumField() {
		field := local.Field(i)
		mirror, ok := remote.FieldByName(field.Name)
		if !ok || field.Type != mirror.Type || field.Tag.Get("json") != mirror.Tag.Get("json") {
			t.Fatalf("mutation wire drift: %s", field.Name)
		}
	}
	t.Chdir(t.TempDir())
	sidecarMutationRecorder{kind: "issue"}.RecordExternalRef(context.Background(), providers.ExternalRef{
		Provider: providers.ProviderGitHub, Ref: "team/repo#7", Operation: "claim-release",
		RunID: "lease-owner", Outcome: "failure", ErrorCode: "provider_claim_failed", ProviderRunID: "provider-owner", URL: "https://example.invalid/issues/7",
	})
	data, err := os.ReadFile(mutationsSidecarFile)
	if err != nil {
		t.Fatal(err)
	}
	var surrendered dispatcher.SurrenderedMutation
	if err := json.Unmarshal(data, &surrendered); err != nil {
		t.Fatal(err)
	}
	if surrendered.RunID != "lease-owner" || surrendered.Outcome != "failure" || surrendered.ErrorCode != "provider_claim_failed" || surrendered.ID != "7" {
		t.Fatalf("sidecar lost claim outcome: %+v", surrendered)
	}
	if surrendered.ProviderRunID != "provider-owner" || surrendered.Provider != "github" || surrendered.Kind != "issue" || surrendered.Operation != "claim-release" || surrendered.URL != "https://example.invalid/issues/7" {
		t.Fatalf("populated wire fields lost: %+v", surrendered)
	}
}
