package main

import (
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/executor"
)

func TestCommentWatchVersionBoundary(t *testing.T) {
	for _, tc := range []struct {
		version string
		stage   bool
		guarded bool
		fail    bool
	}{
		{"2.0", true, false, false}, {"2.1", true, true, false}, {"3.0", true, true, false}, {"3.1", true, true, false},
		{"", true, false, true}, {"2.2", true, false, true}, {" 2.0", true, false, true}, {"", false, true, false},
	} {
		t.Run(tc.version+"/"+map[bool]string{true: "stage", false: "standalone"}[tc.stage], func(t *testing.T) {
			t.Setenv(executor.DSLVersionEnvVar, tc.version)
			t.Setenv(executor.RunIDEnvVar, "")
			t.Setenv(executor.WorkflowEnvVar, "")
			t.Setenv(executor.TaskEnvVar, "")
			if tc.stage {
				t.Setenv(executor.RunIDEnvVar, "run")
			}
			t.Setenv(executor.InputEnvVar("dslVersion"), "2.0") // authored inputs are irrelevant
			got, err := commentWatchRoutingBudgetEnabled()
			if (err != nil) != tc.fail || got != tc.guarded {
				t.Fatalf("guarded=%v error=%v", got, err)
			}
		})
	}
}

func TestPRCommentWatchMissingOrUnknownVersionCannotDisarmOrWriteADO(t *testing.T) {
	for _, version := range []string{"", "2.2"} {
		t.Run(version, func(t *testing.T) {
			root, fake, _ := adoCommentWatchFixture(t, func(f *fakeADOCommentWatch) {
				pr := f.addPR(42, "goobers/implementation/x", mergeReadyLabel)
				pr.autoComplete = true
				pr.threads = []map[string]any{adoWatchThread(1, adoWatchComment(1, adoWatchHumanID, "please fix", 1))}
			})
			t.Setenv(executor.DSLVersionEnvVar, version)
			t.Setenv(executor.RunIDEnvVar, "run")
			t.Setenv(executor.InputEnvVar("blockUnansweredFeedback"), "true")
			code, _, stderr := runArgs(t, "pr-comment-watch", root)
			if code == 0 || !strings.Contains(stderr, "DSL version") {
				t.Fatalf("code=%d stderr=%s", code, stderr)
			}
			if writes := fake.takeWrites(); len(writes) != 0 {
				t.Fatalf("mutated before version check: %v", writes)
			}
			if !fake.prs[42].autoComplete || len(fake.prs[42].threads) != 1 {
				t.Fatal("changed landing or posted comment before refusal")
			}
		})
	}
}

func TestPRCommentWatchLegacyADOHasNoBudgetWritesOrCap(t *testing.T) {
	root, fake, dir := adoCommentWatchFixture(t, func(f *fakeADOCommentWatch) {
		pr := f.addPR(42, "goobers/implementation/x")
		pr.threads = []map[string]any{adoWatchThread(1, adoWatchComment(1, adoWatchSelfID, "please fix", 1))}
	})
	t.Setenv(executor.DSLVersionEnvVar, "2.0")
	for i := 0; i < commentWatchMaxRoutes+2; i++ {
		runADOCommentWatch(t, root)
		if result := readCommentWatchResult(t, dir); result.Labeled != 1 || result.Parked != 0 {
			t.Fatalf("route %d: %+v", i, result)
		}
		fake.mu.Lock()
		if len(fake.prs[42].threads) != 1 {
			fake.mu.Unlock()
			t.Fatal("legacy 2.0 posted a budget comment")
		}
		for id, label := range fake.prs[42].labels {
			if label == needsRemediationLabel {
				delete(fake.prs[42].labels, id)
			}
		}
		fake.mu.Unlock()
	}
}

func TestPRCommentWatchLegacySharedGitHubIgnoresBudgetWithoutWriting(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addOpenPR(1, "goobers/implementation/x", "main", "sha1", "base1", false, nil, nil)
	server.addIssue(1, "Add feature")
	at := time.Now().UTC().Add(-time.Hour)
	server.addRawCommentAtAsType(1, "goobers", "User", "please fix", at)
	server.addCommentAtAs(1, "goobers", renderCommentWatchBudget(commentWatchBudget{HeadSHA: "sha1", Routes: commentWatchMaxRoutes}), at.Add(time.Minute))
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	dir := commentWatchWorkDir(t)
	t.Setenv(executor.DSLVersionEnvVar, "2.0")
	t.Setenv(executor.InputEnvVar("identityMode"), "shared")
	if code, _, stderr := runArgs(t, "pr-comment-watch", root); code != 0 {
		t.Fatalf("code=%d: %s", code, stderr)
	}
	if result := readCommentWatchResult(t, dir); result.Labeled != 1 || result.Parked != 0 {
		t.Fatalf("result=%+v", result)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.issues[1].comments) != 2 {
		t.Fatalf("legacy comment count=%d", len(server.issues[1].comments))
	}
}
