package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

type fakePendingCICancelProvider struct {
	providers.Provider
	result  providers.CancelPendingChecksResult
	request providers.CancelPendingChecksRequest
}

func (f *fakePendingCICancelProvider) Kind() providers.ProviderKind { return providers.ProviderGitHub }

func (f *fakePendingCICancelProvider) Capabilities() providers.CapabilitySet {
	return providers.NewCapabilitySet(providers.CapCICancel)
}

func (f *fakePendingCICancelProvider) CancelPendingChecks(_ context.Context, request providers.CancelPendingChecksRequest) (providers.CancelPendingChecksResult, error) {
	f.request = request
	return f.result, nil
}

func TestRunCancelPendingCIWritesExactResultShapes(t *testing.T) {
	tests := []struct {
		name       string
		provider   *fakePendingCICancelProvider
		wantJSON   string
		wantStdout string
	}{
		{
			name:       "empty",
			provider:   &fakePendingCICancelProvider{result: providers.CancelPendingChecksResult{Examined: 0}},
			wantJSON:   "{\n  \"status\": \"no-pending-checks\",\n  \"examined\": 0,\n  \"headSha\": \"abc123\",\n  \"pullNumber\": \"42\"\n}\n",
			wantStdout: "no pending CI runs remain for PR #42 at abc123\n",
		},
		{
			name: "partial",
			provider: &fakePendingCICancelProvider{result: providers.CancelPendingChecksResult{
				Examined: 4,
				Canceled: []string{"run-1"},
				Skipped:  []string{"run-2", "run-3"},
			}},
			wantJSON:   "{\n  \"status\": \"completed\",\n  \"examined\": 4,\n  \"canceled\": [\n    \"run-1\"\n  ],\n  \"skipped\": [\n    \"run-2\",\n    \"run-3\"\n  ],\n  \"headSha\": \"abc123\",\n  \"pullNumber\": \"42\"\n}\n",
			wantStdout: "canceled 1 pending CI run(s) for PR #42 at abc123\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resultFile := filepath.Join(t.TempDir(), "result.json")
			setRoutedGitHubRepo(t)
			t.Setenv("GOOBERS_INPUT_RESULTFILE", resultFile)
			t.Setenv("GOOBERS_INPUT_PULLNUMBER", "42")
			t.Setenv("GOOBERS_INPUT_HEADSHA", "abc123")
			t.Setenv("GOOBERS_INPUT_MAXRUNS", "7")

			var stdout, stderr bytes.Buffer
			var gotRoot string
			var gotRepo providers.RepositoryRef
			code := runCancelPendingCIWithProvider([]string{"/instance"}, &stdout, &stderr,
				func(root string, repo providers.RepositoryRef) (providers.Provider, error) {
					gotRoot, gotRepo = root, repo
					return test.provider, nil
				})
			if code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr = %q", code, stderr.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
			if stdout.String() != test.wantStdout {
				t.Fatalf("stdout = %q, want %q", stdout.String(), test.wantStdout)
			}
			data, err := os.ReadFile(resultFile)
			if err != nil {
				t.Fatalf("read result file: %v", err)
			}
			if string(data) != test.wantJSON {
				t.Fatalf("result file =\n%s\nwant exact shape =\n%s", data, test.wantJSON)
			}
			wantRepo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "widgets"}
			if gotRoot != "/instance" || gotRepo != wantRepo {
				t.Fatalf("provider factory got root=%q repo=%+v, want root=/instance repo=%+v", gotRoot, gotRepo, wantRepo)
			}
			wantRequest := providers.CancelPendingChecksRequest{Repository: wantRepo, PullID: "42", HeadSHA: "abc123", Limit: 7}
			if test.provider.request != wantRequest {
				t.Fatalf("CancelPendingChecks request = %+v, want %+v", test.provider.request, wantRequest)
			}
		})
	}
}

func setRoutedGitHubRepo(t *testing.T) {
	t.Helper()
	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderGitHub))
	t.Setenv(executor.RepoOwnerEnvVar, "acme")
	t.Setenv(executor.RepoNameEnvVar, "widgets")
}
