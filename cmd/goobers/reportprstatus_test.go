package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

func TestParseStatusState(t *testing.T) {
	cases := map[string]providers.CheckState{
		"succeeded": providers.CheckStatePassing,
		"success":   providers.CheckStatePassing,
		"passing":   providers.CheckStatePassing,
		"failed":    providers.CheckStateFailing,
		"failure":   providers.CheckStateFailing,
		"failing":   providers.CheckStateFailing,
		"pending":   providers.CheckStatePending,
		"":          providers.CheckStatePending,
	}
	for in, want := range cases {
		got, err := parseStatusState(in)
		if err != nil {
			t.Fatalf("parseStatusState(%q) error: %v", in, err)
		}
		if got != want {
			t.Fatalf("parseStatusState(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := parseStatusState("bogus"); err == nil {
		t.Fatal("parseStatusState accepted an unknown state")
	}
}

// report-pr-status is an Azure DevOps parity feature: a GitHub-routed run must
// fail with an actionable error rather than silently succeeding.
func TestReportPRStatusRejectsGitHubProvider(t *testing.T) {
	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderGitHub))
	t.Setenv(executor.RepoOwnerEnvVar, "your-org")
	t.Setenv(executor.RepoNameEnvVar, "your-repo")
	t.Setenv(executor.InputEnvVar("prNumber"), "42")

	var stdout, stderr bytes.Buffer
	if code := runReportPRStatus([]string{t.TempDir()}, &stdout, &stderr); code != 1 {
		t.Fatalf("code = %d, want 1; stderr = %q", code, stderr.String())
	}
	if !bytes.Contains(stderr.Bytes(), []byte("does not support")) {
		t.Fatalf("stderr = %q, want an unsupported-provider error", stderr.String())
	}
}

func TestReportPRStatusRequiresPRNumber(t *testing.T) {
	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderADO))
	t.Setenv(executor.RepoOwnerEnvVar, "example-org")
	t.Setenv(executor.RepoProjectEnvVar, "Example Service")
	t.Setenv(executor.RepoNameEnvVar, "Example.Repo")

	var stdout, stderr bytes.Buffer
	if code := runReportPRStatus([]string{t.TempDir()}, &stdout, &stderr); code != 1 {
		t.Fatalf("code = %d, want 1; stderr = %q", code, stderr.String())
	}
	if !bytes.Contains(stderr.Bytes(), []byte("prNumber is required")) {
		t.Fatalf("stderr = %q, want a prNumber-required error", stderr.String())
	}
}

// TestReportPRStatusHeadShaPinsTheStatus proves the optional headSha input
// reaches the provider: on Azure DevOps a status whose evidence covers an
// older commit than the pull request's latest iteration is refused, and one
// that covers the latest iteration is posted.
func TestReportPRStatusHeadShaPinsTheStatus(t *testing.T) {
	for _, tc := range []struct {
		name     string
		headSha  string
		wantCode int
	}{
		{name: "reviewed head is current", headSha: "current-head", wantCode: 0},
		{name: "head moved after the evidence", headSha: "reviewed-head", wantCode: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := initDemo(t)
			setNonGitHubStageEnv(t, providers.ProviderADO)
			t.Setenv(executor.InputEnvVar("prNumber"), "77")
			t.Setenv(executor.InputEnvVar("headSha"), tc.headSha)
			t.Setenv(executor.InputEnvVar("resultFile"), filepath.Join(t.TempDir(), "status-result.json"))
			posted := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pullrequests/77/iterations"):
					_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]any{
						{"id": 1, "sourceRefCommit": map[string]string{"commitId": "reviewed-head"}},
						{"id": 2, "sourceRefCommit": map[string]string{"commitId": "current-head"}},
					}})
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pullrequests/77/iterations/2/statuses"):
					posted = true
					_ = json.NewEncoder(w).Encode(map[string]int{"id": 11})
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			pointADOStageProviderAt(t, server)

			code, _, stderr := runArgs(t, "report-pr-status", root)
			if code != tc.wantCode {
				t.Fatalf("code = %d, want %d; stderr = %q", code, tc.wantCode, stderr)
			}
			if posted != (tc.wantCode == 0) {
				t.Fatalf("status posted = %v, want %v", posted, tc.wantCode == 0)
			}
			if tc.wantCode != 0 && !strings.Contains(stderr, "head moved from reviewed-head to current-head") {
				t.Fatalf("stderr = %q, want the head-moved refusal", stderr)
			}
		})
	}
}
