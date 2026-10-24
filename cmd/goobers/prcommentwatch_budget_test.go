package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
)

func TestPRCommentWatchADOSharedIdentityBudget(t *testing.T) {
	root, fake, dir := adoCommentWatchFixture(t, func(f *fakeADOCommentWatch) {
		pr := f.addPR(42, "goobers/implementation/x")
		pr.threads = []map[string]any{adoWatchThread(1, adoWatchComment(1, adoWatchSelfID, "unmarked agent or human feedback", 1))}
	})
	// Simulate remediation completing without changing the head or posting a
	// bound acknowledgement. Each scan is a new command invocation.
	for i := 0; i < commentWatchMaxRoutes; i++ {
		runADOCommentWatch(t, root)
		if got := readCommentWatchResult(t, dir); got.Labeled != 1 || got.Parked != 0 {
			t.Fatalf("route %d: %+v", i, got)
		}
		fake.mu.Lock()
		for id, label := range fake.prs[42].labels {
			if label == needsRemediationLabel {
				delete(fake.prs[42].labels, id)
			}
		}
		fake.mu.Unlock()
	}
	runADOCommentWatch(t, root)
	if got := readCommentWatchResult(t, dir); got.Labeled != 0 || got.Parked != 1 {
		t.Fatalf("exhaustion: %+v", got)
	}
	if labels := fake.labelNames(42); !hasAnyLabel(labels, []string{providers.LabelNeedsHuman}) || !hasAnyLabel(labels, []string{remediationEscalatedLabel}) || hasAnyLabel(labels, []string{needsRemediationLabel}) {
		t.Fatalf("park labels: %v", labels)
	}
	if got := len(fake.prs[42].threads); got != commentWatchMaxRoutes+2 {
		t.Fatalf("threads=%d, want original feedback, three reservations, one escalation", got)
	}
	runADOCommentWatch(t, root)
	if got := len(fake.prs[42].threads); got != commentWatchMaxRoutes+2 {
		t.Fatalf("repeated escalation posted another comment: %d", got)
	}
	// A genuinely new head resets the budget and permits the existing human
	// feedback to route again, clearing the human-decision park labels.
	fake.prs[42].headSHA = "new-head"
	runADOCommentWatch(t, root)
	if got := readCommentWatchResult(t, dir); got.Labeled != 1 || got.Unparked != 1 {
		t.Fatalf("new head: %+v", got)
	}
}

func TestPRCommentWatchSharedGitHubBudgetParks(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addOpenPR(1, "goobers/implementation/x", "main", "sha1", "base1", false, nil, nil)
	server.addIssue(1, "Add feature")
	at := time.Now().UTC().Add(-time.Hour)
	server.addRawCommentAtAsType(1, "goobers", "User", "please fix", at)
	server.addCommentAtAs(1, "goobers", renderCommentWatchBudget(commentWatchBudget{HeadSHA: "sha1", Routes: commentWatchMaxRoutes}), at.Add(time.Minute))
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_IDENTITYMODE", "shared")
	dir := commentWatchWorkDir(t)
	if code, _, stderr := runArgs(t, "pr-comment-watch", root); code != 0 {
		t.Fatalf("code=%d: %s", code, stderr)
	}
	if got := readCommentWatchResult(t, dir); got.Parked != 1 || got.Labeled != 0 {
		t.Fatalf("result=%+v", got)
	}
	if !server.issueHasLabel(1, providers.LabelNeedsHuman) || !server.issueHasLabel(1, remediationEscalatedLabel) || server.issueHasLabel(1, needsRemediationLabel) {
		t.Fatalf("labels=%v", server.issueLabels(1))
	}
}

func TestCommentWatchBudgetProvenanceAndFeedback(t *testing.T) {
	classifier := newCommentClassifier(adoCommentIdentity(providers.ADOIdentity{ID: "self"}), commentIdentitySettings{mode: commentIdentityShared})
	body := renderCommentWatchBudget(commentWatchBudget{HeadSHA: "head", Routes: commentWatchMaxRoutes})
	for _, tc := range []struct {
		name, author, body, head string
		want                     int
	}{
		{"self", "self", body, "head", 3},
		{"other author cannot spend budget", "other", body, "head", 0},
		{"different head", "self", body, "new-head", 0},
		{"quoted", "self", "> " + strings.ReplaceAll(body, "\n", "\n> "), "head", 0},
		{"fenced", "self", "```\n" + body + "\n```", "head", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readCommentWatchBudget([]providers.Comment{{AuthorID: tc.author, Body: tc.body}}, classifier, tc.head)
			if err != nil || got.Routes != tc.want {
				t.Fatalf("state=%+v err=%v", got, err)
			}
		})
	}
	human := providers.Comment{AuthorID: "self", Body: "unmarked feedback"}
	state := providers.Comment{AuthorID: "self", Body: body}
	if got, fresh, _ := latestUnaddressedHumanComment([]providers.Comment{human, state}, classifier); !fresh || got.Body != human.Body {
		t.Fatal("budget record acknowledged unmarked shared-identity human feedback")
	}
}

type budgetFailForge struct {
	prCommentWatchForge
	recorded int
	err      error
}

func (f *budgetFailForge) recordRouting(context.Context, providers.PullRequestSummary, string) error {
	f.recorded++
	return f.err
}

func TestCommentWatchBudgetRequiresDurableReservation(t *testing.T) {
	classifier := newCommentClassifier(loginCommentIdentity("self"), commentIdentitySettings{mode: commentIdentityShared})
	f := &budgetFailForge{err: errors.New("post failed")}
	if _, err := reserveCommentWatchRoute(t.Context(), f, providers.PullRequestSummary{HeadSHA: "head"}, nil, classifier); err == nil {
		t.Fatal("reservation failure allowed routing")
	}
	if _, err := reserveCommentWatchRoute(t.Context(), f, providers.PullRequestSummary{}, nil, classifier); err == nil || f.recorded != 1 {
		t.Fatalf("missing head should fail before writing: writes=%d err=%v", f.recorded, err)
	}
}

func TestPRCommentWatchADOBudgetDisarmsLandingBeforeParking(t *testing.T) {
	root, fake, dir := adoCommentWatchFixture(t, func(f *fakeADOCommentWatch) {
		pr := f.addPR(42, "goobers/implementation/x", mergeReadyLabel)
		pr.autoComplete = true
		budget := adoWatchThread(2, adoWatchComment(1, adoWatchSelfID, renderCommentWatchBudget(commentWatchBudget{HeadSHA: "head-sha", Routes: commentWatchMaxRoutes}), 2))
		budget["status"] = "closed"
		pr.threads = []map[string]any{adoWatchThread(1, adoWatchComment(1, adoWatchSelfID, "please fix", 1)), budget}
	})
	t.Setenv("GOOBERS_INPUT_BLOCKUNANSWEREDFEEDBACK", "true")
	runADOCommentWatch(t, root)
	if got := readCommentWatchResult(t, dir); got.Parked != 1 || got.Labeled != 0 {
		t.Fatalf("result=%+v", got)
	}
	if fake.prs[42].autoComplete {
		t.Fatal("landing remained armed")
	}
	writes := fake.takeWrites()
	if len(writes) < 3 || writes[0] != "disarm:42" {
		t.Fatalf("must disarm before parking: %v", writes)
	}
	if labels := fake.labelNames(42); hasAnyLabel(labels, []string{mergeReadyLabel, needsRemediationLabel}) {
		t.Fatalf("park retained automatic routing labels: %v", labels)
	}
}
