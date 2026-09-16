package worktree

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/workspacerevision"
)

func TestExactRevisionOptionsFailClosed(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for name, mutate := range map[string]func(*CreateOptions){
		"moving base":     func(o *CreateOptions) { o.BaseRef = "main" },
		"branch":          func(o *CreateOptions) { o.Branch = "topic" },
		"sync base":       func(o *CreateOptions) { o.SyncBase = true },
		"existing branch": func(o *CreateOptions) { o.RequireExistingBranch = true },
		"acquire branch":  func(o *CreateOptions) { o.AcquireRemoteBranch = true },
		"short SHA":       func(o *CreateOptions) { o.ExpectedSHA = "abcd"; o.BaseRef = "abcd" },
		"missing source":  func(o *CreateOptions) { o.RepoURL = "" },
		"revision syntax": func(o *CreateOptions) { o.ExpectedSHA = sha + "^{commit}"; o.BaseRef = o.ExpectedSHA },
	} {
		t.Run(name, func(t *testing.T) {
			opts := CreateOptions{RepoURL: "authorized", RunID: "read", BaseRef: sha, ExpectedSHA: sha}
			mutate(&opts)
			// Invalid controls must be rejected before any filesystem or Git use.
			_, err := (&Manager{}).Create(context.Background(), opts)
			assertRevisionCode(t, err, workspacerevision.CodeInvalid)
		})
	}
}

func TestPinnedRevisionRequiresPinnedWorkspace(t *testing.T) {
	wt := &Worktree{}
	assertRevisionCode(t, wt.PreparePinnedRevision(context.Background(), "source", strings.Repeat("a", 40), nil), workspacerevision.CodeInvalid)
	assertRevisionCode(t, wt.ResetPinnedRevision(context.Background(), strings.Repeat("a", 40)), workspacerevision.CodeInvalid)
}

func assertRevisionCode(t *testing.T, err error, code string) {
	t.Helper()
	var revisionErr *workspacerevision.Error
	if !errors.As(err, &revisionErr) || revisionErr.Code != code {
		t.Fatalf("error = %v, want %s", err, code)
	}
}
