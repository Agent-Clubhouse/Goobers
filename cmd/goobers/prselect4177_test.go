package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// TestPRSelectStopsReReviewingAnUnchangedAdvisoryPR drives Goobers#4177.
//
// An advisory cycle is terminal (advisory-verdict's pass branch is "") and
// read-only (apply-verdict makes no GitHub mutation for it), so nothing it
// does makes its subject ineligible on the next tick. Fairness only rotates
// when some OTHER PR is aging behind it: with one advisory PR the lane
// re-selects the same unchanged head every tick, forever.
//
// That is not merely wasted spend. Live, merge-review re-reviewed #4150
// eighteen consecutive times at an unchanged head and exhausted the GitHub
// REST quota, which then failed pr-select outright
// ("github_rate_limited: detect foundation-coupled pull requests: list files
// for PR #4167", run e63866ce62c389b71dff289f18bf9fc4) — an unrelated PR's
// selection broken by another PR's repeat review.
func TestPRSelectStopsReReviewingAnUnchangedAdvisoryPR(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	const advisoryPR = 10

	server.addIssue(advisoryPR, "Advisory PR outside headPrefixes")
	server.addOpenPR(advisoryPR, "copilot/outside-prefix", "main", "head-advisory", "base-sha", false, nil, nil)

	routeMergeReviewTestRepo(t)

	first := selectOnePullRequest(t, server, root, "run-4177-first")
	if first["number"] != "10" {
		t.Fatalf("first selection = %q, want advisory PR #10", first["number"])
	}
	if first["advisoryMode"] != "true" {
		t.Fatalf("first selection advisoryMode = %q, want true", first["advisoryMode"])
	}

	releasePullRequestClaimForTest(t, root, advisoryPR, "run-4177-first")

	second, stdout := selectPullRequest(t, server, root, "run-4177-second")
	if second != nil {
		t.Fatalf("second selection = %q, want no work: the advisory verdict for this head is already published",
			second["number"])
	}
	if !strings.Contains(stdout, "advisory verdict already published") {
		t.Fatalf("pr-select stdout = %q, want the advisory-suppression diagnostic", stdout)
	}
}

// TestPRSelectAdvisorySuppressionIsKeyedByHeadSHA pins the two properties that
// keep the #4177 suppression from becoming a permanent silence: a new commit
// is advised again immediately, and a recorded-but-lost verdict is retried
// once the repeat interval elapses.
func TestPRSelectAdvisorySuppressionIsKeyedByHeadSHA(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	advised := providers.PullRequestSummary{Number: 4150, HeadSHA: "head-a"}
	now := time.Now().UTC()

	if err := recordPRSelectAdvisory(root, repo, advised, now); err != nil {
		t.Fatal(err)
	}

	heads, err := loadPRSelectAdvisedHeads(root, repo, now)
	if err != nil {
		t.Fatal(err)
	}
	if !advisoryAlreadyDispatched(heads, advised) {
		t.Fatal("the advised head SHA is not suppressed")
	}
	repushed := providers.PullRequestSummary{Number: 4150, HeadSHA: "head-b"}
	if advisoryAlreadyDispatched(heads, repushed) {
		t.Fatal("a new head SHA must be advised again, not suppressed")
	}
	unknownHead := providers.PullRequestSummary{Number: 4150}
	if advisoryAlreadyDispatched(heads, unknownHead) {
		t.Fatal("an empty head SHA must never suppress a PR permanently")
	}

	expired, err := loadPRSelectAdvisedHeads(root, repo, now.Add(prSelectAdvisoryRepeatInterval+time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if advisoryAlreadyDispatched(expired, advised) {
		t.Fatal("suppression must lapse after the repeat interval so a lost verdict is retried")
	}
}

func selectOnePullRequest(t *testing.T, server *fakeGitHubServer, root, runID string) map[string]string {
	t.Helper()
	selected, stdout := selectPullRequest(t, server, root, runID)
	if selected == nil {
		t.Fatalf("pr-select selected nothing: %s", stdout)
	}
	return selected
}

func selectPullRequest(t *testing.T, server *fakeGitHubServer, root, runID string) (map[string]string, string) {
	t.Helper()
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", runID)
	t.Setenv(executor.InputEnvVar("authorScope"), authorScopeAny)
	t.Setenv(executor.InputEnvVar("headPrefixes"), "goobers/implementation/")
	dir := t.TempDir()
	t.Chdir(dir)
	code, selectStdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, selectStdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(dir, "selected-pr.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, selectStdout
	}
	if err != nil {
		t.Fatalf("read selected-pr.json: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal selected-pr.json: %v", err)
	}
	if noWork, _ := raw[executor.OutputNoWork].(bool); noWork {
		return nil, selectStdout
	}
	selected := make(map[string]string, len(raw))
	for key, value := range raw {
		text, ok := value.(string)
		if !ok {
			t.Fatalf("selected-pr.json[%q] = %v, want a string", key, value)
		}
		selected[key] = text
	}
	return selected, selectStdout
}

func releasePullRequestClaimForTest(t *testing.T, root string, number int, runID string) {
	t.Helper()
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layoutFor(root).SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	key := localscheduler.ClaimKey{
		Gaggle:     "goobers",
		Provider:   string(providers.ProviderGitHub),
		ExternalID: pullRequestClaimKey(number),
	}
	if err := ledger.ReleaseScoped(key, runID); err != nil {
		t.Fatal(err)
	}
}
