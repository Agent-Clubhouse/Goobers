package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/providers"
)

func TestVerdictPublicationCustodySerializesFileAndPlane(t *testing.T) {
	for _, modes := range []string{"file/file", "plane/plane", "file/plane", "plane/file"} {
		t.Run(modes, func(t *testing.T) {
			plane := newClaimsPlane(t)
			local, err := fileClaimLedger(plane.layout)
			if err != nil {
				t.Fatal(err)
			}
			first := claimsclient.MergeLock{Key: claimsclient.MergeLockKey("test", "github", "publication/github/acme", "repo"), RunID: "first", Workflow: "merge-review"}
			second := first
			second.RunID = "second"
			clients := []claimsclient.Ledger{local, local}
			for i, mode := range strings.Split(modes, "/") {
				run := []string{"first", "second"}[i]
				token := plane.admitRun(t, "test", run)
				if mode == "plane" {
					clients[i] = plane.client(t, run, token)
				}
			}
			entered, release := make(chan struct{}), make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- withPublicationClaim(t.Context(), clients[0], first, time.Second, 5*time.Second, 10*time.Second, func(context.Context) error {
					close(entered)
					<-release
					return nil
				})
			}()
			<-entered
			err = withPublicationClaim(t.Context(), clients[1], second, 80*time.Millisecond, time.Second, 10*time.Second, func(context.Context) error { return errors.New("entered occupied custody") })
			close(release)
			if heldErr := <-done; heldErr != nil {
				t.Fatal(heldErr)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("contending publisher entered landing custody: %v", err)
			}
			enteredAfter := false
			err = withPublicationClaim(t.Context(), clients[1], second, time.Second, time.Second, 10*time.Second, func(context.Context) error { enteredAfter = true; return nil })
			if err != nil || !enteredAfter {
				t.Fatalf("custody not released: %v", err)
			}
		})
	}
}

func TestVerdictPublicationCustodyRetainsLeaseOnUncertainCompletion(t *testing.T) {
	plane := newClaimsPlane(t)
	ledger, err := fileClaimLedger(plane.layout)
	if err != nil {
		t.Fatal(err)
	}
	lock := claimsclient.MergeLock{Key: claimsclient.MergeLockKey("test", "github", "publication/github/acme", "repo"), RunID: "first", Workflow: "merge-review"}
	err = withPublicationClaim(t.Context(), ledger, lock, time.Second, 20*time.Millisecond, time.Second, func(ctx context.Context) error { <-ctx.Done(); return nil })
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "outcome uncertain") {
		t.Fatalf("uncertain completion reported as success: %v", err)
	}
	retryErr := withPublicationClaim(t.Context(), ledger, lock, 40*time.Millisecond, 100*time.Millisecond, time.Second, func(context.Context) error {
		t.Error("same-run retry bypassed the retained quarantine")
		return nil
	})
	if !errors.Is(retryErr, context.DeadlineExceeded) {
		t.Fatalf("same-run retry did not wait: %v", retryErr)
	}
	ok, holder, err := ledger.ClaimScoped(t.Context(), lock.Key, "second", "merge-review", time.Second)
	if err != nil || ok || holder != "first" {
		t.Fatalf("uncertain landing released its quarantine: ok=%t holder=%q err=%v", ok, holder, err)
	}
}

func TestVerdictPublicationCustodyDetectsLossAfterAction(t *testing.T) {
	plane := newClaimsPlane(t)
	ledger, err := fileClaimLedger(plane.layout)
	if err != nil {
		t.Fatal(err)
	}
	lock := claimsclient.MergeLock{Key: claimsclient.MergeLockKey("test", "github", "publication/github/acme", "repo"), RunID: "first", Workflow: "merge-review"}
	err = withPublicationClaim(t.Context(), ledger, lock, time.Second, time.Second, 5*time.Second, func(ctx context.Context) error {
		e := ledger.ReleaseScoped(ctx, lock.Key, lock.RunID, claimrelease.Completed.Because("verdict_publication_closed", ""))
		return e
	})
	if err == nil || !strings.Contains(err.Error(), "custody lost") {
		t.Fatalf("lost custody reported as success: %v", err)
	}
}

func TestApplyVerdictVoidPreservesUnmigratedSameHeadFindings(t *testing.T) {
	const head, base, run = "reviewed-head", "reviewed-base", "review-run"
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(10, "Selected PR")
	server.addOpenPR(10, "goobers/implementation/run-10", "main", head, "moved-base", false, nil, nil)
	server.addComment(10, renderVerdictComment(apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "Unanswered", HeadSHA: head, BaseSHA: base, Findings: []apiv1.Finding{unboundedLineageFinding()}}))
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", run)
	t.Setenv("GOOBERS_CRED_GITHUB_PR_REVIEW", "review-token")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "10")
	t.Setenv("GOOBERS_INPUT_SELECTEDHEADSHA", head)
	t.Setenv("GOOBERS_INPUT_SELECTEDBASESHA", base)
	seedGateVerdictJournal(t, root, run, apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "Stale pass", HeadSHA: head, BaseSHA: base})
	t.Chdir(t.TempDir())
	if code, out, stderr := runArgs(t, "apply-verdict", root); code != 0 || !strings.Contains(out, "verdict void") {
		t.Fatalf("void: %d %s %s", code, out, stderr)
	}
	comments, _ := fakeIssueComments(t, server, 10)
	if len(comments) != 1 || !strings.Contains(comments[0], "verdict: stale") {
		t.Fatalf("not staled: %v", comments)
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	records, err := verdictRecordsForPR(root, repo, 10, []providers.Comment{{Author: server.authenticatedLogin, Body: comments[0]}}, server.authenticatedLogin)
	if err != nil {
		t.Fatal(err)
	}
	publication, err := beginVerdictPublication(root, repo, 10)
	if err != nil {
		t.Fatal(err)
	}
	next, err := publication.commit(apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "Silent pass", HeadSHA: head, BaseSHA: "moved-base"}, records, nil, io.Discard)
	if err != nil || next.Decision != apiv1.VerdictNeedsChanges || len(next.StandingFindings) != 1 {
		t.Fatalf("void lost the finding: %+v %v", next, err)
	}
}
