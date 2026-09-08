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
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/providers"
)

func TestTelemetryMergesReportsRealJournalConfirmation(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	id := strings.Repeat("a", 32)
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{RunID: "merge-report", Workflow: "landing", Gaggle: "web", InstanceID: id}, nil, journal.WithClock(func() time.Time { return at }))
	if err != nil {
		t.Fatal(err)
	}
	confirmation := &providers.MergeConfirmation{RepositoryAPIURL: "https://forge.example/repos/acme/app", PullID: "9", MergeSHA: "commit"}
	intent := &providers.LandingIntent{ID: strings.Repeat("b", 32), Operation: "merge", RepositoryAPIURL: confirmation.RepositoryAPIURL, PullID: "11", ExpectedHeadSHA: "head"}
	if err := run.Append(journal.Event{Type: journal.EventRefTouched, ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "11"}, Runner: providers.MutationRunnerFields("merge-intent", nil, nil, intent)}); err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRefTouched, ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "9"}, Runner: providers.MutationRunnerFields("merge", confirmation, nil, nil)}); err != nil {
		t.Fatal(err)
	}
	admission := &providers.QueueAdmission{RepositoryAPIURL: confirmation.RepositoryAPIURL, PullID: "10", EntryID: "owned-entry", ExpectedHeadSHA: "queue-head", EnqueuedAt: at}
	if err := run.Append(journal.Event{Type: journal.EventRefTouched, ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "10"}, Runner: providers.MutationRunnerFields("enqueue", nil, admission, nil)}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(layout.TelemetryDB()), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.IngestRun(context.Background(), run.Dir()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runArgs(t, "telemetry", "merges", "--json", "--since=2026-09-01T00:00:00Z", "--until=2026-09-02T00:00:00Z", layout.Root)
	if code != 0 {
		t.Fatalf("report failed: %d %s", code, stderr)
	}
	var report rollup.MergeReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Merges) != 1 || report.Merges[0].InstanceID != id || report.Merges[0].PullID != "9" || len(report.Daily) != 1 || report.Daily[0].Count != 1 {
		t.Fatalf("CLI lost provenance: %+v", report)
	}
	if len(report.QueueAdmissions) != 1 || report.QueueAdmissions[0].EntryID != "owned-entry" || report.QueueAdmissions[0].PullID != "10" || report.QueueAdmissions[0].InstanceID != id || report.QueueAdmissions[0].RunID != "merge-report" {
		t.Fatalf("CLI lost queue ownership: %+v", report.QueueAdmissions)
	}
	if len(report.LandingIntents) != 1 || report.LandingIntents[0].LandingIntent != *intent || report.LandingIntents[0].InstanceID != id {
		t.Fatalf("CLI lost persisted attempt: %+v", report.LandingIntents)
	}
	code, stdout, stderr = runArgs(t, "telemetry", "merges", "--since=2026-09-01T00:00:00Z", "--until=2026-09-02T00:00:00Z", layout.Root)
	if code != 0 || !strings.Contains(stdout, "Persisted landing attempts (not merge proof): 1") || !strings.Contains(stdout, intent.ID) || !strings.Contains(stdout, "Confirmed merges: 1;") {
		t.Fatalf("human report conflated attempts and merges: %d %s %s", code, stdout, stderr)
	}
}

func TestTelemetryMergesComparesRealForgeWithoutLosingFilteredFleetProof(t *testing.T) {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer read-only-token" {
			t.Errorf("unexpected authority or mutation: %s %s", r.Method, r.URL)
		}
		var response any
		switch r.URL.Path {
		case "/repos/acme/app/pulls":
			response = []map[string]any{{"number": 9, "updated_at": at, "merged_at": at}, {"number": 10, "updated_at": at, "merged_at": at}}
		case "/repos/acme/app/pulls/9":
			response = map[string]any{"number": 9, "merged": true, "merged_at": at, "merge_commit_sha": "commit", "merged_by": map[string]string{"login": "shared"}}
		case "/repos/acme/app/pulls/10":
			response = map[string]any{"number": 10, "merged": true, "merged_at": at, "merged_by": map[string]string{"login": "SHARED"}, "user": map[string]string{"login": "external-author"}}
		default:
			t.Errorf("unexpected endpoint %s", r.URL)
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	previous := newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = previous })
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		return providers.NewGitHubProvider(token, append(opts, func(p *providers.GitHubProvider) { p.BaseURL = server.URL })...)
	}
	t.Setenv(executor.CredentialEnvVar(string(capability.GitHubPRRead)), "read-only-token")
	t.Setenv(executor.CredentialEnvVar(string(capability.GitHubPRWrite)), "")
	layout := instance.NewLayout(t.TempDir())
	id := strings.Repeat("a", 32)
	// A delayed durable receipt must remain proof for the forge's merge-time
	// window even when the receipt itself is recorded two days later.
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{RunID: "forge-report", Workflow: "landing", Gaggle: "other-fleet", InstanceID: id}, nil, journal.WithClock(func() time.Time { return at.Add(48 * time.Hour) }))
	if err != nil {
		t.Fatal(err)
	}
	confirmation := &providers.MergeConfirmation{RepositoryAPIURL: server.URL + "/repos/acme/app", PullID: "9", MergeSHA: "commit"}
	if err := run.Append(journal.Event{Type: journal.EventRefTouched, ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "9"}, Runner: providers.MutationRunnerFields("merge", confirmation, nil, nil)}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(layout.TelemetryDB()), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.IngestRun(context.Background(), run.Dir()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runArgs(t, "telemetry", "merges", "--json", "--gaggle=selected-fleet", "--compare-github=acme/app", "--shared-identities=shared", "--since=2026-09-01T00:00:00Z", "--until=2026-09-02T00:00:00Z", layout.Root)
	if code != 0 {
		t.Fatalf("comparison failed: %d %s", code, stderr)
	}
	var report rollup.MergeReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Merges) != 0 || report.Comparison == nil || len(report.Comparison.Entries) != 2 {
		t.Fatalf("bad filtered report: %+v", report)
	}
	entries := report.Comparison.Entries
	if entries[0].Category != "daemon-verified" || entries[0].InstanceID != id || entries[1].Category != "same-identity-unverified" || entries[1].Gaggle != "" || len(report.Comparison.Daily) != 2 {
		t.Fatalf("forge comparison lost proof or invented attribution: %+v", report.Comparison)
	}
}

func TestTelemetryMergesReadCredentialIsExplicitAndDoesNotInheritWriteToken(t *testing.T) {
	t.Setenv("MERGE_REPORT_WRITE_TEST", "write-token")
	t.Setenv("MERGE_REPORT_READ_TEST", "read-token")
	cfg := &instance.Config{Repos: []instance.RepoRef{{Provider: "github", Owner: "acme", Name: "app", Token: instance.TokenRef{Env: "MERGE_REPORT_WRITE_TEST"}}}}
	resolver, grants, err := buildCredentials(cfg, nil, "", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolveGrants(t, resolver, grants); got[string(capability.GitHubPRRead)] != "" {
		t.Fatal("read inventory silently inherits repo write token")
	}
	cfg.Credentials = []instance.CredentialGrant{{Capability: string(capability.GitHubPRRead), Token: instance.TokenRef{Env: "MERGE_REPORT_READ_TEST"}}}
	resolver, grants, err = buildCredentials(cfg, nil, "", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := resolveGrants(t, resolver, grants)
	if got[string(capability.GitHubPRRead)] != "read-token" || got[string(capability.GitHubPRWrite)] != "write-token" {
		t.Fatalf("read grant not isolated from write grant: %v", got)
	}
}
