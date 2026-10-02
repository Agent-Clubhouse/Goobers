package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/providers"
)

// TestADOStageProviderEmitsRateLimitDelayTelemetry is the ADO-N40 wiring
// contract: an ADO stage provider (built the same way the daemon builds one
// for a running stage) carries a stage rate-limit observer, so
// X-RateLimit-Delay reaches the stage telemetry sidecar the soak-pass
// criterion (docs/design/ado-parity-dsl-2-0.md §8.3) reads — the same way
// GitHub's stage provider already does (TestProviderCommandEmitsRateLimitTelemetry).
func TestADOStageProviderEmitsRateLimitDelayTelemetry(t *testing.T) {
	const credential = "ado-rate-limit-token-canary"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+credential {
			t.Fatalf("Authorization = %q", got)
		}
		if strings.Contains(r.URL.Path, "/workitemtypes/") {
			_, _ = w.Write([]byte(`{"value":[{"name":"Active","category":"InProgress"}]}`))
			return
		}
		w.Header().Set("X-RateLimit-Delay", "1.5")
		writeADOTestWorkItem(w)
	}))
	defer server.Close()

	dir := telemetry.PrepareStageTelemetryDir(t.TempDir())
	t.Setenv(telemetry.StageTelemetryEnv, dir)

	env := stageEnvFor(map[string]string{executor.RepoAuthSchemeEnvVar: "bearer"})
	source, err := stageADOCredentialSourceFrom(env, capability.RepoPush, credential)
	if err != nil {
		t.Fatalf("stageADOCredentialSource: %v", err)
	}
	provider, err := buildADOProviderForStage(
		providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "project", Name: "repo"},
		source,
	)
	if err != nil {
		t.Fatalf("buildADOProviderForStage: %v", err)
	}
	provider.BaseURL = server.URL

	if _, err := provider.GetWorkItem(context.Background(), providers.RepositoryRef{Project: "project"}, "42"); err != nil {
		t.Fatalf("GetWorkItem() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), credential) {
		t.Fatalf("rate-limit telemetry leaked provider credential: %s", data)
	}
	var event struct {
		Name  string         `json:"name"`
		Attrs map[string]any `json:"attrs"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatal(err)
	}
	if event.Name != telemetry.ProviderRateLimitEventName ||
		event.Attrs["provider"] != "ado" ||
		event.Attrs["outcome"] != "delayed" {
		t.Fatalf("rate-limit telemetry event = %#v", event)
	}
}

func writeADOTestWorkItem(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":42,"fields":{"System.WorkItemType":"Issue","System.Title":"delayed","System.State":"Active"}}`))
}
