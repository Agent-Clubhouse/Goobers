package main

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/goobers/goobers/providers"
)

type requeueMutationRecorder struct {
	mu   sync.Mutex
	refs []providers.ExternalRef
}

func (r *requeueMutationRecorder) RecordExternalRef(_ context.Context, ref providers.ExternalRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refs = append(r.refs, ref)
}

func (r *requeueMutationRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.refs)
}

func TestBacklogQueryRequeuesIssueAfterUnmergedPRClosure(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Retry this implementation",
		"goobers:approved", "goobers:ready", inReviewStatusLabel)
	server.addOpenPR(101, "goobers/implementation/prior-run", "main", "head", "base", false, nil, nil)
	server.setPRBody(101, "Implements the requested fix.\n\nFixes #7")
	server.setPRClosed(101)

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-2")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	t.Setenv("GOOBERS_INPUT_EXCLUDELABELS", inReviewStatusLabel)
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "claimed 7") {
		t.Fatalf("stdout = %q, want issue 7 reclaimed after its PR closed unmerged", stdout)
	}

	server.mu.Lock()
	issue := server.issues[7]
	labels := append([]string(nil), issue.labels...)
	server.mu.Unlock()
	if hasAllLabels(labels, []string{inReviewStatusLabel}) {
		t.Fatalf("issue labels = %v, want %q cleared", labels, inReviewStatusLabel)
	}
}

func TestClosedPRReconciliationIsMergeSafeAndIdempotent(t *testing.T) {
	tests := []struct {
		name          string
		merge         bool
		wantInReview  bool
		wantMutations int
	}{
		{
			name:          "closed unmerged",
			wantMutations: 1,
		},
		{
			name:         "merged",
			merge:        true,
			wantInReview: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newFakeGitHubServer(t, "acme", "app")
			server.addIssue(7, "Implement safely",
				"goobers:approved", "goobers:ready", inReviewStatusLabel)
			server.addOpenPR(101, "goobers/implementation/run-1", "main", "head", "base", false, nil, nil)
			server.setPRBody(101, "Fixes #7")
			if tt.merge {
				server.setPRMerged(101)
			} else {
				server.setPRClosed(101)
			}

			recorder := &requeueMutationRecorder{}
			provider := server.newGitHubProvider("token", providers.WithMutationRecorder(recorder))
			repo := providers.RepositoryRef{
				Provider: providers.ProviderGitHub,
				Owner:    "acme",
				Name:     "app",
			}

			for observation := 0; observation < 2; observation++ {
				if err := reconcileClosedUnmergedInReview(
					context.Background(), provider, repo, map[string]bool{},
				); err != nil {
					t.Fatalf("observation %d: %v", observation+1, err)
				}
			}

			server.mu.Lock()
			issue := server.issues[7]
			labels := append([]string(nil), issue.labels...)
			comments := append([]string(nil), issue.comments...)
			server.mu.Unlock()
			if got := hasAllLabels(labels, []string{inReviewStatusLabel}); got != tt.wantInReview {
				t.Fatalf("in-review label present = %v, want %v; labels = %v", got, tt.wantInReview, labels)
			}
			if got := recorder.count(); got != tt.wantMutations {
				t.Fatalf("mutation count after repeated observation = %d, want %d", got, tt.wantMutations)
			}
			if len(comments) != 0 {
				t.Fatalf("comments after repeated observation = %v, want none", comments)
			}
		})
	}
}
