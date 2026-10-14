package providers

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// canceledBuildFake is a pull request whose required build policy ADO
// rejected because its build was canceled before it ran: the outage shape of
// #6817 on Azure DevOps (no agent was ever assigned).
func canceledBuildFake(t *testing.T) *adoBuildFake {
	f := rejectedBuildFake(t, "head-sha", 6)
	f.builds["314"]["result"] = "canceled"
	f.timelines["314"] = []map[string]interface{}{
		timelineRecord("job-linux", "", "Job", "Linux", "canceled", 1, 0),
	}
	return f
}

func pollADOFake(t *testing.T, provider *ADOProvider) PullRequestPollResult {
	t.Helper()
	result, err := provider.PollPullRequest(context.Background(), PullRequestPollRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"},
		PullID:     "42",
	})
	if err != nil {
		t.Fatalf("PollPullRequest: %v", err)
	}
	return result
}

// TestADOCanceledBuildIsNotFailingCI is #6817's Azure DevOps acceptance: a
// build policy rejected for a canceled build polls as pending (never passing,
// so no merge; never failing, so no remediation), gather-pr-context's
// hasFailingCI read is false, and gather-ci-failures reports nothing for an
// agent to fix.
func TestADOCanceledBuildIsNotFailingCI(t *testing.T) {
	f := canceledBuildFake(t)
	provider, done := f.serve()
	defer done()
	repo := RepositoryRef{Name: "repo", Project: "project"}

	poll := pollADOFake(t, provider)
	if poll.CheckState != CheckStatePending {
		t.Fatalf("CheckState = %q, want pending for a canceled build", poll.CheckState)
	}
	check := checkNamed(t, poll.Checks, "Build")
	if check.State != CheckStatePending || check.Summary != "build canceled before a verdict; waiting for a rerun" {
		t.Fatalf("check = %+v, want pending and saying the build was canceled", check)
	}

	failing, err := provider.HasPullRequestCIFailures(context.Background(), repo, "42")
	if err != nil || failing {
		t.Fatalf("HasPullRequestCIFailures = %v, %v; want false", failing, err)
	}
	if failures := f.collect(t, provider); len(failures) != 0 {
		t.Fatalf("PullRequestCIFailures = %+v, want none for a canceled build", failures)
	}
	if r := f.requested("/org/project/_apis/build/builds/314/timeline"); r != nil {
		t.Fatalf("read %s: a canceled build has no failure evidence to collect", r.URL.Path)
	}
}

// TestADOCanceledBuildBesideAFailureStillFails pins that a canceled build
// hides nothing: another rejected build that failed still fails CI.
func TestADOCanceledBuildBesideAFailureStillFails(t *testing.T) {
	f := canceledBuildFake(t)
	f.evaluations = append(f.evaluations, buildPolicy("rejected", 315))
	f.builds["315"] = prBuild(315, "head-sha")
	provider, done := f.serve()
	defer done()

	if poll := pollADOFake(t, provider); poll.CheckState != CheckStateFailing {
		t.Fatalf("CheckState = %q, want failing for the failed build beside the canceled one", poll.CheckState)
	}
	failures := f.collect(t, provider)
	if len(failures) != 1 || !strings.HasSuffix(failures[0].URL, "/_build/results?buildId=315") {
		t.Fatalf("failures = %+v, want only the failed build 315", failures)
	}
}

// TestADOUnreadableBuildKeepsTheRejection pins the fallback: when the build
// behind a rejected policy cannot be read (a credential without build read
// access, say), the evaluation's own verdict stands, as before #6817, rather
// than the read failing the poll.
func TestADOUnreadableBuildKeepsTheRejection(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusNotFound} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			f := canceledBuildFake(t)
			f.failPaths["/org/project/_apis/build/builds/314"] = code
			provider, done := f.serve()
			defer done()

			if poll := pollADOFake(t, provider); poll.CheckState != CheckStateFailing {
				t.Fatalf("CheckState = %q, want failing: an unread build proves no cancellation", poll.CheckState)
			}
			failing, err := provider.HasPullRequestCIFailures(context.Background(), RepositoryRef{Name: "repo", Project: "project"}, "42")
			if err != nil || !failing {
				t.Fatalf("HasPullRequestCIFailures = %v, %v; want true", failing, err)
			}
		})
	}
}
