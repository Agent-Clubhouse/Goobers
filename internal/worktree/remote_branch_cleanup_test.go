package worktree

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workspacerevision"
)

func TestRemoteBranchCleanupRefusesMissingAuthority(t *testing.T) {
	opts := RemoteBranchOptions{TargetURL: filepath.Join(t.TempDir(), "uncontacted.git"), Binding: apiv1.WorkspaceBranchBinding{
		Repository: apiv1.RepositoryIdentity{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "base"},
		Ref:        "refs/heads/ns/workflow/run", StartingSHA: strings.Repeat("a", 40),
	}}
	for _, tc := range []struct {
		name, expected string
		outcome        RemoteBranchCleanupOutcome
		errorCode      string
	}{
		{"no-tip", "", CleanupOwnershipMissing, ""},
		{"malformed-tip", "main", CleanupOwnershipInvalid, workspacerevision.CodeInvalid},
		{"no-target-grant", opts.Binding.StartingSHA, CleanupFailed, workspacerevision.CodeUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := CleanupRemoteBranch(context.Background(), opts, tc.expected)
			if result.Outcome != tc.outcome || (err != nil) != (tc.errorCode != "") {
				t.Fatalf("cleanup = %+v, %v", result, err)
			}
			if tc.errorCode != "" {
				var refusal *workspacerevision.Error
				if !errors.As(err, &refusal) || refusal.Code != tc.errorCode {
					t.Fatalf("cleanup did not refuse before remote access: %v", err)
				}
			}
		})
	}
}
