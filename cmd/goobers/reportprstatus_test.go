package main

import (
	"bytes"
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
