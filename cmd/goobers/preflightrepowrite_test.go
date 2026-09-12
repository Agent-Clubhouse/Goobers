package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/goobers/goobers/providers"
)

type fakeRepoWritePreflightProvider struct {
	providers.Provider
	result providers.RepositoryWritePreflightResult
	repo   providers.RepositoryRef
	branch string
}

func (f *fakeRepoWritePreflightProvider) Kind() providers.ProviderKind {
	return providers.ProviderGitHub
}

func (f *fakeRepoWritePreflightProvider) Capabilities() providers.CapabilitySet {
	return providers.NewCapabilitySet(providers.CapRepoPushPreflight)
}

func (f *fakeRepoWritePreflightProvider) PreflightRepositoryWrite(_ context.Context, repo providers.RepositoryRef, branch string) (providers.RepositoryWritePreflightResult, error) {
	f.repo, f.branch = repo, branch
	return f.result, nil
}

func TestRunPreflightRepoWriteDispatchesAndReportsBehavior(t *testing.T) {
	tests := []struct {
		name       string
		result     providers.RepositoryWritePreflightResult
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{
			name:       "pushable",
			result:     providers.RepositoryWritePreflightResult{OK: true},
			wantCode:   0,
			wantStdout: "repository-write preflight: acme/widgets can push \"custom/run-9\"\n",
		},
		{
			name: "branch policy refusal",
			result: providers.RepositoryWritePreflightResult{
				FailureCapability: providers.RepoWriteFailureBranchPolicy,
				Detail:            "ruleset blocks custom/run-9",
			},
			wantCode:   1,
			wantStderr: "error: repository-write preflight failed: repo=acme/widgets capability=repo.push.branch-policy: ruleset blocks custom/run-9\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setRoutedGitHubRepo(t)
			t.Setenv("GOOBERS_INPUT_BRANCH", "custom/run-9")
			fake := &fakeRepoWritePreflightProvider{result: test.result}
			var stdout, stderr bytes.Buffer
			var gotRoot string
			code := runPreflightRepoWriteWithProvider([]string{"/instance"}, &stdout, &stderr,
				func(root string, _ providers.RepositoryRef) (providers.Provider, error) {
					gotRoot = root
					return fake, nil
				})
			if code != test.wantCode {
				t.Fatalf("exit code = %d, want %d", code, test.wantCode)
			}
			if stdout.String() != test.wantStdout {
				t.Fatalf("stdout = %q, want %q", stdout.String(), test.wantStdout)
			}
			if stderr.String() != test.wantStderr {
				t.Fatalf("stderr = %q, want %q", stderr.String(), test.wantStderr)
			}
			wantRepo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "widgets"}
			if gotRoot != "/instance" || fake.repo != wantRepo || fake.branch != "custom/run-9" {
				t.Fatalf("dispatch got root=%q repo=%+v branch=%q, want /instance, %+v, custom/run-9", gotRoot, fake.repo, fake.branch, wantRepo)
			}
		})
	}
}
