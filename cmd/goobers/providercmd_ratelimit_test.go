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

	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/providers"
)

func TestProviderCommandEmitsRateLimitTelemetry(t *testing.T) {
	const credential = "github-rate-limit-token-canary"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+credential {
			t.Fatalf("Authorization = %q", got)
		}
		w.Header().Set("Retry-After", "1")
		http.Error(w, "secondary rate limit", http.StatusForbidden)
	}))
	defer server.Close()

	dir := telemetry.PrepareStageTelemetryDir(t.TempDir())
	t.Setenv(telemetry.StageTelemetryEnv, dir)
	provider := newTelemetryGitHubProvider(credential,
		func(p *providers.GitHubProvider) { p.BaseURL = server.URL },
		providers.WithMaxRateLimitRetries(0),
	)
	_, err := provider.GetWorkItem(context.Background(), providers.RepositoryRef{Owner: "acme", Name: "app"}, "7")
	if err == nil {
		t.Fatal("GetWorkItem() error = nil, want exhausted rate-limit error")
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
		event.Attrs["provider"] != "github" ||
		event.Attrs["outcome"] != "exhausted" {
		t.Fatalf("rate-limit telemetry event = %#v", event)
	}
	if scope, _ := event.Attrs["scope"].(string); scope == "" || strings.Contains(scope, "?") {
		t.Fatalf("rate-limit telemetry scope = %q", scope)
	}
}
