package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// Full-length SHAs: the revision contract (#6128) refuses abbreviated or
// placeholder heads, so these fixtures use real-shaped object names.
const (
	revisionSelectedSHA  = "1111111111111111111111111111111111111111"
	revisionMovedSHA     = "2222222222222222222222222222222222222222"
	revisionPublishedSHA = "3333333333333333333333333333333333333333"
)

func TestIsFullCommitSHA(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{revisionSelectedSHA, true},
		{strings.ToUpper("abcdef0123456789abcdef0123456789abcdef01"), true},
		{strings.Repeat("a", 64), true},
		{"abc1234", false},
		{"head-sha", false},
		{"", false},
		{strings.Repeat("g", 40), false},
	} {
		if got := isFullCommitSHA(tc.in); got != tc.want {
			t.Errorf("isFullCommitSHA(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestEvaluatePRRevision is the contract table every provider shares: the
// provider only supplies a PullRequestSummary, and the classification of that
// summary is one function, so GitHub, Gitea and Azure DevOps cannot disagree.
func TestEvaluatePRRevision(t *testing.T) {
	expected := prExpectedRevision{PullRequest: "77", ExpectedHeadSHA: revisionSelectedSHA, Source: prRevisionSourceSelection}
	open := func(head string) providers.PullRequestSummary {
		return providers.PullRequestSummary{Number: 77, State: "open", HeadSHA: head}
	}
	for _, tc := range []struct {
		name     string
		recorded bool
		pr       providers.PullRequestSummary
		want     prRevisionState
		wantErr  bool
	}{
		{name: "matching head is current", recorded: true, pr: open(revisionSelectedSHA), want: prRevisionCurrent},
		{name: "case-insensitive match", recorded: true, pr: open(strings.ToUpper(revisionSelectedSHA)), want: prRevisionCurrent},
		{name: "moved head is stale", recorded: true, pr: open(revisionMovedSHA), want: prRevisionStale},
		{name: "merged is terminal whatever its head", recorded: true, pr: providers.PullRequestSummary{State: "closed", Merged: true, HeadSHA: revisionMovedSHA}, want: prRevisionTerminal},
		{name: "closed is terminal", recorded: true, pr: providers.PullRequestSummary{State: "closed"}, want: prRevisionTerminal},
		{name: "open but merged is terminal", recorded: true, pr: providers.PullRequestSummary{State: "open", Merged: true}, want: prRevisionTerminal},
		{name: "missing live head fails closed", recorded: true, pr: open(""), wantErr: true},
		{name: "missing live head fails closed without a record", recorded: false, pr: open("  "), wantErr: true},
		{name: "abbreviated live head fails closed", recorded: true, pr: open("1111111"), wantErr: true},
		{name: "unrecorded open PR", recorded: false, pr: open(revisionMovedSHA), want: prRevisionUnrecorded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := evaluatePRRevision(expected, tc.recorded, tc.pr)
			if tc.wantErr {
				if !errors.Is(err, errPRRevisionUnverifiable) {
					t.Fatalf("err = %v, want errPRRevisionUnverifiable", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("evaluatePRRevision: %v", err)
			}
			if got.State != tc.want {
				t.Fatalf("state = %q, want %q", got.State, tc.want)
			}
		})
	}
}

// revisionRun is a run journal a test can keep appending to between stage
// invocations — the same journal a resumed or retried stage re-reads.
type revisionRun struct {
	t     *testing.T
	runID string
	run   *journal.Run
}

func newRevisionRun(t *testing.T, root, runID string) *revisionRun {
	t.Helper()
	run, err := journal.Create(layoutFor(root).RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "pr-remediation", Gaggle: "goobers",
	}, nil)
	if err != nil {
		t.Fatalf("create run journal: %v", err)
	}
	t.Cleanup(func() { _ = run.Close() })
	return &revisionRun{t: t, runID: runID, run: run}
}

// selectBrief records gather-pr-context's result artifact exactly as the
// executor does: <runID>:<stage>/result.
func (r *revisionRun) selectBrief(brief any) {
	r.t.Helper()
	data, err := json.Marshal(brief)
	if err != nil {
		r.t.Fatalf("marshal brief: %v", err)
	}
	if _, err := r.run.RecordArtifact(r.runID+":gather-pr-context/result", data); err != nil {
		r.t.Fatalf("record selection: %v", err)
	}
}

func (r *revisionRun) selectHead(number, head string) {
	r.t.Helper()
	brief := reviewThreadsBrief()
	brief.SelectedNumber = number
	brief.GatherPRContext.HeadSHA = head
	r.selectBrief(brief)
}

func (r *revisionRun) publish(localHead string) {
	r.t.Helper()
	if err := r.run.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "push-remediated", Attempt: 1, Status: string(apiv1.ResultSuccess),
		Outputs: map[string]any{pushRemediatedPublishedOutput: "true", pushRemediatedLocalHeadOutput: localHead},
	}); err != nil {
		r.t.Fatalf("append push-remediated: %v", err)
	}
}

// rebasePush records rebase-pr's clean-rebase force-push on the agentic path
// (failing-ci / human-comment), leased against attempted.
func (r *revisionRun) rebasePush(attempted, pushed string) {
	r.t.Helper()
	if err := r.run.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "rebase-pr", Attempt: 1, Status: string(apiv1.ResultSuccess),
		Outputs: map[string]any{"needsAgent": "true", "attemptedHeadSha": attempted, rebasePushedHeadOutput: pushed},
	}); err != nil {
		r.t.Fatalf("append rebase-pr: %v", err)
	}
}

func TestLoadPRExpectedRevision(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "acme", Project: "project", Name: "web"}
	for _, tc := range []struct {
		name       string
		seed       func(r *revisionRun)
		wantHead   string
		wantSource string
		unrecorded bool
		wantErr    bool
	}{
		{name: "selection records the head", seed: func(r *revisionRun) { r.selectHead("77", revisionSelectedSHA) },
			wantHead: revisionSelectedSHA, wantSource: prRevisionSourceSelection},
		{name: "own publication advances the expectation", seed: func(r *revisionRun) {
			r.selectHead("77", revisionSelectedSHA)
			r.publish(revisionPublishedSHA)
		}, wantHead: revisionPublishedSHA, wantSource: prRevisionSourcePublication},
		{name: "a later selection resets it", seed: func(r *revisionRun) {
			r.selectHead("77", revisionSelectedSHA)
			r.publish(revisionPublishedSHA)
			r.selectHead("77", revisionMovedSHA)
		}, wantHead: revisionMovedSHA, wantSource: prRevisionSourceSelection},
		{name: "unpublished push does not advance", seed: func(r *revisionRun) {
			r.selectHead("77", revisionSelectedSHA)
			_ = r.run.Append(journal.Event{Type: journal.EventStageFinished, Stage: "push-remediated", Outputs: map[string]any{
				pushRemediatedPublishedOutput: "false", pushRemediatedLocalHeadOutput: revisionPublishedSHA,
			}})
		}, wantHead: revisionSelectedSHA, wantSource: prRevisionSourceSelection},
		// rebase-pr force-pushes a clean rebase and continues into the agentic
		// chain on a failing-ci / human-comment cycle: that push is this run's
		// own, so the guards after it must not read it as a stale selection.
		{name: "own rebase push advances the expectation", seed: func(r *revisionRun) {
			r.selectHead("77", revisionSelectedSHA)
			r.rebasePush(revisionSelectedSHA, revisionPublishedSHA)
		}, wantHead: revisionPublishedSHA, wantSource: prRevisionSourceRebase},
		// rebase-pr leases against the head IT checked out: a human push after
		// selection would be rebased and published under that lease, so it
		// must not be adopted.
		{name: "rebase push over a foreign head does not advance", seed: func(r *revisionRun) {
			r.selectHead("77", revisionSelectedSHA)
			r.rebasePush(revisionMovedSHA, revisionPublishedSHA)
		}, wantHead: revisionSelectedSHA, wantSource: prRevisionSourceSelection},
		{name: "rebase without a push does not advance", seed: func(r *revisionRun) {
			r.selectHead("77", revisionSelectedSHA)
			r.rebasePush(revisionSelectedSHA, "")
		}, wantHead: revisionSelectedSHA, wantSource: prRevisionSourceSelection},
		{name: "no selection is unrecorded", seed: func(*revisionRun) {}, unrecorded: true},
		{name: "a no-work selection records nothing", seed: func(r *revisionRun) {
			r.selectBrief(map[string]any{"claimed": false, "noWork": true})
		}, unrecorded: true},
		{name: "selection of another PR fails closed", seed: func(r *revisionRun) { r.selectHead("78", revisionSelectedSHA) }, wantErr: true},
		{name: "placeholder head fails closed", seed: func(r *revisionRun) { r.selectHead("77", "head-sha") }, wantErr: true},
		{name: "empty head fails closed", seed: func(r *revisionRun) { r.selectHead("77", "") }, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := initDemo(t)
			r := newRevisionRun(t, root, "run-6128")
			tc.seed(r)
			got, recorded, err := loadPRExpectedRevision(root, "run-6128", repo, 77)
			if tc.wantErr {
				if !errors.Is(err, errPRRevisionUnverifiable) {
					t.Fatalf("err = %v, want errPRRevisionUnverifiable", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadPRExpectedRevision: %v", err)
			}
			if recorded == tc.unrecorded {
				t.Fatalf("recorded = %v, want %v", recorded, !tc.unrecorded)
			}
			if got.ExpectedHeadSHA != tc.wantHead || got.Source != tc.wantSource {
				t.Fatalf("expected = %+v, want head %q from %q", got, tc.wantHead, tc.wantSource)
			}
			if got.Provider != providers.ProviderADO || got.Repository != repo.CanonicalKey() || got.PullRequest != "77" {
				t.Fatalf("identity = %+v, want the claimed ADO PR", got)
			}
		})
	}
}

// TestLoadPRExpectedRevisionReadsPreExistingBriefVersions pins resume
// compatibility: a run whose gather-pr-context wrote a v1/v2 brief before this
// binary deployed still yields its selected head — gatherPrContext.headSha is
// required in every brief wire version, so no new field is needed to recover it.
func TestLoadPRExpectedRevisionReadsPreExistingBriefVersions(t *testing.T) {
	for _, schema := range apiv1.SupportedRemediationBriefVersions() {
		t.Run(schema, func(t *testing.T) {
			root := initDemo(t)
			r := newRevisionRun(t, root, "run-old")
			r.selectBrief(map[string]any{
				"schema": schema, "selectedNumber": "77", "head": "work", "base": "main", "workspaceBranch": "work",
				"isBehindBase": false, "hasSubstantiveFindings": "true", "hasFailingCI": "false",
				"gatherPrContext": map[string]any{"headSha": revisionSelectedSHA, "baseSha": "b", "verdict": nil, "comments": []any{}},
			})
			got, recorded, err := loadPRExpectedRevision(root, "run-old", prClaimTestRepo(), 77)
			if err != nil || !recorded || got.ExpectedHeadSHA != revisionSelectedSHA {
				t.Fatalf("expected = %+v, recorded=%v, err=%v; want the old brief's selected head", got, recorded, err)
			}
		})
	}
}

func TestLoadPRExpectedRevisionWithoutRunJournalIsUnrecorded(t *testing.T) {
	root := initDemo(t)
	_, recorded, err := loadPRExpectedRevision(root, "no-such-run", prClaimTestRepo(), 77)
	if err != nil || recorded {
		t.Fatalf("recorded=%v err=%v; want an unrecorded expectation for a run with no local journal", recorded, err)
	}
}

// claimedPRFake is one provider's fake for the claimed PR #77, whose head and
// state a test moves between stage invocations.
type claimedPRFake struct {
	root  string
	runID string
	set   func(head, state string)
}

func newClaimedPRFake(t *testing.T, kind providers.ProviderKind, head string) claimedPRFake {
	t.Helper()
	switch kind {
	case providers.ProviderGitHub:
		return newGitHubClaimedPRFake(t, head)
	case providers.ProviderGitea:
		return newGiteaClaimedPRFake(t, head)
	case providers.ProviderADO:
		return newADOClaimedPRFake(t, head)
	}
	t.Fatalf("unsupported provider %q", kind)
	return claimedPRFake{}
}

func newGitHubClaimedPRFake(t *testing.T, head string) claimedPRFake {
	st := &remediationCheckpointServerState{number: 77, state: "open", headSHA: head}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)
	root := remediationCheckpointEnv(t, server.URL, false)
	seedRevisionClaim(t, root, prClaimTestRepo(), "run-364")
	return claimedPRFake{root: root, runID: "run-364", set: func(head, state string) {
		st.mu.Lock()
		defer st.mu.Unlock()
		st.headSHA, st.state, st.merged = head, state, state == "merged"
		if state == "merged" {
			st.state = "closed"
		}
	}}
}

func newADOClaimedPRFake(t *testing.T, head string) claimedPRFake {
	root, repo := providerDispatchFixture(t, providers.ProviderADO)
	st := &adoRemediationServerState{
		owner: repo.Owner, project: repo.Project, name: repo.Name,
		prNumber: 77, prBranch: "goobers/impl/ado-claim", base: "main",
		headSHA: head, baseSHA: "base-sha", status: "active",
	}
	installADOStageProvider(t, repo, st.start(t))
	t.Setenv("GOOBERS_RUN_ID", "run-ado-6128")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "delivered-pr-token")
	seedRevisionClaim(t, root, repo, "run-ado-6128")
	return claimedPRFake{root: root, runID: "run-ado-6128", set: func(head, state string) {
		st.mu.Lock()
		defer st.mu.Unlock()
		st.headSHA = head
		switch state {
		case "merged":
			st.status = "completed"
		case "closed":
			st.status = "abandoned"
		default:
			st.status = "active"
		}
	}}
}

func newGiteaClaimedPRFake(t *testing.T, head string) claimedPRFake {
	var mu sync.Mutex
	state, merged := "open", false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/repos/your-org/your-repo/pulls/77" {
			t.Errorf("unexpected Gitea request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		writeFakeJSON(w, map[string]any{
			"number": 77, "state": state, "merged": merged,
			"head": map[string]any{"ref": "goobers/impl/gitea-claim", "sha": head},
			"base": map[string]any{"ref": "main", "sha": "base-sha"},
		})
	}))
	t.Cleanup(server.Close)
	root := initDemo(t)
	configureRemediationGitea(t, root, server.URL)
	t.Setenv("GOOBERS_RUN_ID", "run-gitea-6128")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderGitea))
	t.Setenv(executor.RepoOwnerEnvVar, "your-org")
	t.Setenv(executor.RepoNameEnvVar, "your-repo")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "gitea-pr-token")
	repo := providers.RepositoryRef{Provider: providers.ProviderGitea, Owner: "your-org", Name: "your-repo"}
	seedRevisionClaim(t, root, repo, "run-gitea-6128")
	return claimedPRFake{root: root, runID: "run-gitea-6128", set: func(h, s string) {
		mu.Lock()
		defer mu.Unlock()
		head, state, merged = h, s, s == "merged"
		if s == "merged" {
			state = "closed"
		}
	}}
}

func seedRevisionClaim(t *testing.T, root string, repo providers.RepositoryRef, runID string) {
	t.Helper()
	t.Setenv("GOOBERS_INPUT_RESULTFILE", filepath.Join(t.TempDir(), prRemediationLifecycleResultFile))
	if _, err := claimPullRequestInOrder(root, repo, []providers.PullRequestSummary{{Number: 77}}, runID, "pr-remediation", time.Hour); err != nil {
		t.Fatalf("seed PR claim: %v", err)
	}
}

func readLifecycleResult(t *testing.T) prRemediationLifecycleResult {
	t.Helper()
	raw, err := os.ReadFile(os.Getenv("GOOBERS_INPUT_RESULTFILE"))
	if err != nil {
		t.Fatalf("read pr-claim result: %v", err)
	}
	var result prRemediationLifecycleResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode pr-claim result %s: %v", raw, err)
	}
	return result
}

func prClaimHeld(t *testing.T, root string) bool {
	t.Helper()
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layoutFor(root).SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatalf("open claim ledger: %v", err)
	}
	_, held := ledger.Lookup(pullRequestClaimKey(77))
	return held
}

var revisionProviders = []providers.ProviderKind{providers.ProviderGitHub, providers.ProviderGitea, providers.ProviderADO}

// TestPRClaimRevisionGuardAcrossProviders is #6128's shared semantic contract,
// end to end through `goobers pr-claim` on every provider: a matching head
// keeps the claim across repeated guard invocations (a restart, resume or
// retry re-reads the same journal record, never a live re-selection); a head
// moved between claim and the next guard is a distinct stale-selection
// no-work that releases the claim; and terminal stays terminal.
func TestPRClaimRevisionGuardAcrossProviders(t *testing.T) {
	for _, kind := range revisionProviders {
		t.Run(string(kind), func(t *testing.T) {
			fake := newClaimedPRFake(t, kind, revisionSelectedSHA)
			newRevisionRun(t, fake.root, fake.runID).selectHead("77", revisionSelectedSHA)

			for attempt := 1; attempt <= 2; attempt++ {
				code, stdout, stderr := runArgs(t, "pr-claim", fake.root)
				if code != 0 {
					t.Fatalf("guard %d: code = %d, stdout = %q, stderr = %q", attempt, code, stdout, stderr)
				}
				result := readLifecycleResult(t)
				if !result.Open || result.NoWork || result.Outcome != prClaimOutcomeOpen || result.Revision != string(prRevisionCurrent) ||
					result.ExpectedHeadSHA != revisionSelectedSHA || result.LiveHeadSHA != revisionSelectedSHA {
					t.Fatalf("guard %d result = %+v, want the claim retained at the selected head", attempt, result)
				}
				if !prClaimHeld(t, fake.root) {
					t.Fatalf("guard %d released a current claim", attempt)
				}
			}

			// A human pushes to the PR branch between claim and mutation.
			fake.set(revisionMovedSHA, "open")
			code, stdout, stderr := runArgs(t, "pr-claim", fake.root)
			if code != 0 {
				t.Fatalf("stale guard: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
			result := readLifecycleResult(t)
			if !result.NoWork || !result.StaleSelection || result.Outcome != prClaimOutcomeStaleSelection ||
				result.ExpectedHeadSHA != revisionSelectedSHA || result.LiveHeadSHA != revisionMovedSHA || !result.Released {
				t.Fatalf("stale result = %+v, want a released stale-selection no-work naming both heads", result)
			}
			if !strings.Contains(stdout, "stale selection") || strings.Contains(stdout, "no longer open") {
				t.Fatalf("stdout = %q, want the stale-selection reason, not the terminal one", stdout)
			}
			if prClaimHeld(t, fake.root) {
				t.Fatal("stale selection kept the claim")
			}
		})
	}
}

func TestPRClaimTerminalStaysDistinctFromStaleAcrossProviders(t *testing.T) {
	for _, kind := range revisionProviders {
		t.Run(string(kind), func(t *testing.T) {
			fake := newClaimedPRFake(t, kind, revisionSelectedSHA)
			newRevisionRun(t, fake.root, fake.runID).selectHead("77", revisionSelectedSHA)
			// Merged at a head the run never selected: terminal wins.
			fake.set(revisionMovedSHA, "merged")
			code, stdout, stderr := runArgs(t, "pr-claim", fake.root)
			if code != 0 {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
			result := readLifecycleResult(t)
			if !result.NoWork || result.StaleSelection || result.Outcome != prClaimOutcomeTerminal || result.Open {
				t.Fatalf("result = %+v, want terminal no-work, not a stale selection", result)
			}
			if !strings.Contains(stdout, "no longer open") {
				t.Fatalf("stdout = %q, want the terminal reason", stdout)
			}
		})
	}
}

func TestPRClaimFailsClosedOnUnverifiableHeadAcrossProviders(t *testing.T) {
	for _, kind := range revisionProviders {
		for _, head := range []string{"", "abc1234"} {
			t.Run(string(kind)+"/"+head, func(t *testing.T) {
				fake := newClaimedPRFake(t, kind, revisionSelectedSHA)
				newRevisionRun(t, fake.root, fake.runID).selectHead("77", revisionSelectedSHA)
				fake.set(head, "open")
				code, _, stderr := runArgs(t, "pr-claim", fake.root)
				if code != 1 {
					t.Fatalf("code = %d, stderr = %q; want a fail-closed business error", code, stderr)
				}
				raw, _ := os.ReadFile(os.Getenv("GOOBERS_INPUT_RESULTFILE"))
				if !strings.Contains(string(raw), errorCodePRRevisionUnverifiable) {
					t.Fatalf("result = %s, want typed %s", raw, errorCodePRRevisionUnverifiable)
				}
				if !prClaimHeld(t, fake.root) {
					t.Fatal("an unverifiable head silently released the claim")
				}
			})
		}
	}
}

func TestPRClaimAcceptsOwnPublishedHead(t *testing.T) {
	for _, kind := range revisionProviders {
		t.Run(string(kind), func(t *testing.T) {
			fake := newClaimedPRFake(t, kind, revisionPublishedSHA)
			r := newRevisionRun(t, fake.root, fake.runID)
			r.selectHead("77", revisionSelectedSHA)
			r.publish(revisionPublishedSHA)
			code, stdout, stderr := runArgs(t, "pr-claim", fake.root)
			if code != 0 {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
			result := readLifecycleResult(t)
			if !result.Open || result.RevisionSource != prRevisionSourcePublication || result.ExpectedHeadSHA != revisionPublishedSHA {
				t.Fatalf("result = %+v, want this run's own published head accepted", result)
			}
		})
	}
}

func TestPRClaimProviderErrorKeepsClaim(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	root := remediationCheckpointEnv(t, server.URL, false)
	seedRevisionClaim(t, root, prClaimTestRepo(), "run-364")
	newRevisionRun(t, root, "run-364").selectHead("77", revisionSelectedSHA)
	code, _, _ := runArgs(t, "pr-claim", root)
	if code != 1 {
		t.Fatalf("code = %d, want a provider failure", code)
	}
	if !prClaimHeld(t, root) {
		t.Fatal("a provider error released the claim")
	}
}

// humanPushToPRBranch lands a commit on the PR branch from outside the run —
// the chaos case: a human pushes between claim and publication.
func humanPushToPRBranch(t *testing.T, wtPath string) string {
	t.Helper()
	origin := strings.TrimSpace(runGitOutputT(t, wtPath, "remote", "get-url", "origin"))
	clone := filepath.Join(t.TempDir(), "human")
	runGitT(t, filepath.Dir(clone), "clone", "--branch", remediationPRBranch, origin, clone)
	runGitT(t, clone, "config", "user.name", "human")
	runGitT(t, clone, "config", "user.email", "human@example.com")
	if err := os.WriteFile(filepath.Join(clone, "human.txt"), []byte("pushed mid-remediation\n"), 0o644); err != nil {
		t.Fatalf("write human change: %v", err)
	}
	runGitT(t, clone, "add", "human.txt")
	runGitT(t, clone, "commit", "-m", "human push")
	runGitT(t, clone, "push", "origin", remediationPRBranch)
	return strings.TrimSpace(runGitOutputT(t, clone, "rev-parse", "HEAD"))
}

func readJSONResult(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode result %s: %v", raw, err)
	}
	return out
}

// pushRevisionFixture is a push-remediated fixture on kind whose move func
// simulates remediation-checkpoint having recorded a head the run never
// selected: the live head AND the sticky lease expectation both become head.
func pushRevisionFixture(t *testing.T, kind providers.ProviderKind) (root, wtPath, selected, runID string, move func(head string)) {
	t.Helper()
	if kind == providers.ProviderGitHub {
		var st *remediationCheckpointServerState
		root, st, wtPath, selected = pushRemediatedFixture(t, false)
		return root, wtPath, selected, "run-392-push", func(head string) {
			comment := mustRemediationStateComment(t, head, st.baseSHA)
			st.mu.Lock()
			defer st.mu.Unlock()
			st.headSHA, st.comments = head, []string{comment}
		}
	}
	var st *adoRemediationServerState
	root, st, wtPath, selected = pushRemediatedADOFixture(t, false)
	setDeliveredADOStageCredentials(t)
	return root, wtPath, selected, "run-392-ado", func(head string) {
		comment := mustRemediationStateComment(t, head, st.baseSHA)
		st.mu.Lock()
		defer st.mu.Unlock()
		st.headSHA, st.threadComments = head, []string{comment}
	}
}

func mustRemediationStateComment(t *testing.T, head, base string) string {
	t.Helper()
	comment, err := remediationStateComment(remediationState{Cycles: 1, LastDiffDigest: "sha256:prior", HeadSHA: head, BaseSHA: base})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}
	return comment
}

// TestPushRemediatedRefusesHeadMovedSinceSelection is the case the lease alone
// cannot catch: a human pushed after rebase-pr fetched the branch but before
// remediation-checkpoint recorded the lease expectation, so the recorded
// "pre-remediation" SHA IS the human's commit and a force-with-lease against
// it would succeed — silently discarding that commit. The revision recorded at
// selection is what exposes it (#6128).
func TestPushRemediatedRefusesHeadMovedSinceSelection(t *testing.T) {
	for _, kind := range []providers.ProviderKind{providers.ProviderGitHub, providers.ProviderADO} {
		t.Run(string(kind), func(t *testing.T) {
			root, wtPath, selected, runID, move := pushRevisionFixture(t, kind)
			newRevisionRun(t, root, runID).selectHead("77", selected)
			moved := humanPushToPRBranch(t, wtPath)
			move(moved)

			code, stdout, stderr := runArgs(t, "push-remediated", root)
			if code != 0 {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
			result := readJSONResult(t, filepath.Join(wtPath, pushRemediatedResultName))
			if result["noWork"] != true || result["outcome"] != prClaimOutcomeStaleSelection || result["published"] != "false" ||
				result["expectedHeadSha"] != selected || result["liveHeadSha"] != moved {
				t.Fatalf("push result = %v, want an unpublished stale-selection no-work", result)
			}
			pushed := strings.TrimSpace(runGitOutputT(t, wtPath, "ls-remote", "origin", "refs/heads/"+remediationPRBranch))
			if pushedSHA, _, _ := strings.Cut(pushed, "\t"); pushedSHA != moved {
				t.Fatalf("remote branch = %q, want the human's commit %q left in place", pushedSHA, moved)
			}
			if prClaimHeld(t, root) {
				t.Fatal("stale selection kept the claim")
			}
		})
	}
}

// TestPushRemediatedPublishesAtSelectedHead: the precondition is transparent
// when nothing moved — the existing lease-guarded publish still happens.
func TestPushRemediatedPublishesAtSelectedHead(t *testing.T) {
	root, _, wtPath, selected := pushRemediatedFixture(t, true)
	newRevisionRun(t, root, "run-392-push").selectHead("77", selected)
	code, stdout, stderr := runArgs(t, "push-remediated", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if result := readCheckpointResult(t, filepath.Join(wtPath, pushRemediatedResultName)); result["published"] != "true" {
		t.Fatalf("push result = %v, want published", result)
	}
}
