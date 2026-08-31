package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/decomposition"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// This file covers personal-gaggle-routing §5.1/§5.2/§5.3 for the decomposition
// trio (select-source, validate-plan, publish-batch). Before these, all three
// addressed the routed CODE repository with the project's capability token and
// claimed the parent under a gaggle-scoped key, so on a cross-repository
// backlog they read the wrong container with a token that cannot see the right
// one, and their parent claim did not contend with backlog-query's v3 claim on
// the same item.

const (
	decompositionBacklogOwner = "gim-home"
	decompositionBacklogName  = "brandiv.goobers"
	decompositionProjectName  = "dev-brandiv"
	decompositionConnection   = "private-backlog"
)

func decompositionBacklogIdentity() apiv1.BacklogIdentity {
	return apiv1.BacklogIdentity{
		Provider: apiv1.ProviderGitHub,
		Owner:    decompositionBacklogOwner,
		Name:     decompositionBacklogName,
	}
}

// crossRepositoryDecompositionEnv wires the topology under test at the CLI
// level: the run is routed to project repo gim-home/dev-brandiv, while the
// only server that answers is the BACKLOG repo gim-home/brandiv.goobers behind
// its own connection credential. A stage that addresses the project repo gets
// no handler at all (404), so "reads the right container" is observable rather
// than asserted only against a recorder.
func crossRepositoryDecompositionEnv(t *testing.T, root, runID string, issue int, labels ...string) *fakeGitHubServer {
	t.Helper()
	server := newFakeGitHubServer(t, decompositionBacklogOwner, decompositionBacklogName)
	if issue != 0 {
		server.addIssue(issue, "A very large issue", labels...)
	}
	providerCmdEnv(t, server, executor.CredentialEnvVar(string(capability.GitHubIssuesWrite)), runID)
	// providerCmdEnv routes the stage at the server's repo; re-point the ROUTED
	// repo at the project so the backlog is genuinely a different repository.
	t.Setenv(executor.RepoNameEnvVar, decompositionProjectName)
	setBacklogStageEnv(t, apiv1.BacklogRef{
		Provider:      apiv1.ProviderGitHub,
		Project:       decompositionBacklogOwner + "/" + decompositionBacklogName,
		ConnectionRef: decompositionConnection,
	})
	t.Setenv(executor.CredentialEnvVar(string(capability.GitHubIssuesWrite)), "project-token")
	t.Setenv(executor.CredentialEnvVar(string(capability.GitHubIssuesRead)), "project-token")
	t.Setenv(executor.CredentialEnvVar(credentials.ConnectionCredentialKey(decompositionConnection)), "backlog-token")
	decompositionInstanceEnv(t, root)
	t.Setenv("GOOBERS_RUN_ID", runID)
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", providers.LabelApproved)
	return server
}

func readSelection(t *testing.T, workDir string) decomposition.Selection {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(workDir, "selection.json"))
	if err != nil {
		t.Fatalf("read selection.json: %v", err)
	}
	var selection decomposition.Selection
	if err := json.Unmarshal(data, &selection); err != nil {
		t.Fatalf("unmarshal selection.json: %v", err)
	}
	return selection
}

// TestSelectSourceAddressesBacklogRepositoryAndCredential is the cross-
// repository half of the finding: the parent fetch, its comments, the claim
// marker, and the recorded selection must all name the BACKLOG, and the
// provider must authenticate as spec.backlog.connectionRef rather than with
// the project's capability token.
func TestSelectSourceAddressesBacklogRepositoryAndCredential(t *testing.T) {
	root := t.TempDir()
	buildSelectSourceRun(t, root, selectSourceRunOptions{
		runID:          "escalated-cross-repo",
		startedAt:      time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
		claimedIssueID: "901",
		claimProvider:  "github",
		finalPhase:     journal.PhaseEscalated,
		events:         nonRetryableEscalationEvents("ISSUE_OVER_SCOPE", "too large to implement as one PR"),
	})
	crossRepositoryDecompositionEnv(t, root, "decomposition-run-1", 901, providers.LabelApproved)
	recorded := recordStageProviderConfigs(t)

	workDir := t.TempDir()
	t.Chdir(workDir)
	code, stdout, stderr := runArgs(t, "select-source", root)
	if code != 0 {
		t.Fatalf("select-source: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	selection := readSelection(t, workDir)
	wantRepository := decompositionBacklogOwner + "/" + decompositionBacklogName
	if selection.Parent.ID != "901" || selection.Parent.Repository != wantRepository {
		t.Fatalf("selection parent = %+v, want %s#901", selection.Parent, wantRepository)
	}

	if len(*recorded) != 1 {
		t.Fatalf("recorded %d provider builds, want 1", len(*recorded))
	}
	assertBacklogCredential(t, (*recorded)[0], providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    decompositionBacklogOwner,
		Name:     decompositionBacklogName,
	})

	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", "claims.json"))
	if err != nil {
		t.Fatalf("open claim ledger: %v", err)
	}
	identity := decompositionBacklogIdentity()
	entry, ok := ledger.LookupScoped(backlogClaimKey(identity, "", "901"))
	if !ok || entry.RunID != "decomposition-run-1" {
		t.Fatalf("claim = (%+v, %v), want a backlog-scoped lease held by decomposition-run-1", entry, ok)
	}
	if backlog, scoped := entry.BacklogIdentity(); !scoped || !backlog.Equal(identity) {
		t.Fatalf("claim backlog = %+v (scoped=%v), want %+v", backlog, scoped, identity)
	}
	// The pre-fix key: had select-source kept claiming under the routed code
	// repository's gaggle scope, this is where the lease would have landed —
	// invisible to every backlog-scoped claimant.
	if entry, held := ledger.LookupScoped(localscheduler.ClaimKey{
		Gaggle: "goobers", Provider: "github", ExternalID: "901",
	}); held {
		t.Fatalf("parent was claimed under a gaggle-scoped key: %+v", entry)
	}
}

// TestSelectSourceContendsWithBacklogQueryClaim is the mutual-exclusion half:
// an item already claimed by a backlog-query run — under the v3 backlog key,
// possibly from a DIFFERENT gaggle sharing the same backlog — must not be
// claimable by decomposition, and vice versa. A gaggle-scoped parent claim
// could not see backlog-query's key at all, so both could hold the same item.
func TestSelectSourceContendsWithBacklogQueryClaim(t *testing.T) {
	root := t.TempDir()
	buildSelectSourceRun(t, root, selectSourceRunOptions{
		runID:          "escalated-contended",
		startedAt:      time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
		claimedIssueID: "902",
		claimProvider:  "github",
		finalPhase:     journal.PhaseEscalated,
		events:         nonRetryableEscalationEvents("ISSUE_OVER_SCOPE", "too large"),
	})
	crossRepositoryDecompositionEnv(t, root, "decomposition-run-1", 902, providers.LabelApproved)
	t.Setenv("GOOBERS_GAGGLE", "decomposer")

	identity := decompositionBacklogIdentity()
	ledgerPath := filepath.Join(root, "scheduler", "claims.json")
	if err := os.MkdirAll(filepath.Dir(ledgerPath), 0o755); err != nil {
		t.Fatal(err)
	}
	seed, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatalf("open claim ledger: %v", err)
	}
	// Exactly what `backlog-query --claim` writes, from a sibling gaggle that
	// draws from the same backlog (backlogquery.go's session.claimKey).
	if ok, _, err := seed.ClaimScoped(
		backlogClaimKey(identity, "implementer", "902"),
		"implementation-run-7", "implementation", time.Hour,
	); err != nil || !ok {
		t.Fatalf("seed backlog-query claim: ok=%v err=%v", ok, err)
	}

	workDir := t.TempDir()
	t.Chdir(workDir)
	code, stdout, stderr := runArgs(t, "select-source", root)
	if code != 0 {
		t.Fatalf("select-source: code = %d, stderr = %q", code, stderr)
	}
	assertSelectSourceNoWork(t, stdout, workDir)

	reopened, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatalf("reopen claim ledger: %v", err)
	}
	entry, held := reopened.LookupScoped(backlogClaimKey(identity, "implementer", "902"))
	if !held || entry.RunID != "implementation-run-7" {
		t.Fatalf("backlog-query claim = (%+v, %v), want untouched", entry, held)
	}
}

// TestSelectSourceClaimBlocksLaterBacklogQueryClaim is the converse direction
// of the same exclusion, so neither side can be made to win by construction.
func TestSelectSourceClaimBlocksLaterBacklogQueryClaim(t *testing.T) {
	root := t.TempDir()
	buildSelectSourceRun(t, root, selectSourceRunOptions{
		runID:          "escalated-blocker",
		startedAt:      time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
		claimedIssueID: "903",
		claimProvider:  "github",
		finalPhase:     journal.PhaseEscalated,
		events:         nonRetryableEscalationEvents("ISSUE_OVER_SCOPE", "too large"),
	})
	crossRepositoryDecompositionEnv(t, root, "decomposition-run-1", 903, providers.LabelApproved)
	t.Setenv("GOOBERS_GAGGLE", "decomposer")

	workDir := t.TempDir()
	t.Chdir(workDir)
	if code, _, stderr := runArgs(t, "select-source", root); code != 0 {
		t.Fatalf("select-source: code = %d, stderr = %q", code, stderr)
	}

	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", "claims.json"))
	if err != nil {
		t.Fatalf("open claim ledger: %v", err)
	}
	ok, holder, err := ledger.ClaimScoped(
		backlogClaimKey(decompositionBacklogIdentity(), "implementer", "903"),
		"implementation-run-9", "implementation", time.Hour,
	)
	if err != nil {
		t.Fatalf("competing backlog-query claim: %v", err)
	}
	if ok || holder != "decomposition-run-1" {
		t.Fatalf("competing claim = (ok=%v holder=%q), want refused with decomposition-run-1 holding", ok, holder)
	}
}

// TestValidatePlanReadsBacklogRepository proves validate-plan re-fetches the
// live parent from the backlog. Against the project repo the fetch 404s, so a
// regression surfaces as a provider failure rather than a silent wrong answer;
// where the two repositories happen to number issues alike it would instead
// compare the plan against an unrelated issue and report a bogus conflict.
func TestValidatePlanReadsBacklogRepository(t *testing.T) {
	root := t.TempDir()
	buildSelectSourceRun(t, root, selectSourceRunOptions{
		runID:          "escalated-validate",
		startedAt:      time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
		claimedIssueID: "904",
		claimProvider:  "github",
		finalPhase:     journal.PhaseEscalated,
		events:         nonRetryableEscalationEvents("ISSUE_OVER_SCOPE", "too large"),
	})
	crossRepositoryDecompositionEnv(t, root, "decomposition-run-1", 904, providers.LabelApproved)

	workDir := t.TempDir()
	t.Chdir(workDir)
	if code, _, stderr := runArgs(t, "select-source", root); code != 0 {
		t.Fatalf("select-source: code = %d, stderr = %q", code, stderr)
	}
	plan := validDecompositionPlan(readSelection(t, workDir))
	planData, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "plan.json"), planData, 0o644); err != nil {
		t.Fatal(err)
	}

	recorded := recordStageProviderConfigs(t)
	code, stdout, stderr := runArgs(t, "validate-plan", root)
	if code != 0 {
		t.Fatalf("validate-plan: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if len(*recorded) != 1 {
		t.Fatalf("recorded %d provider builds, want 1", len(*recorded))
	}
	assertBacklogCredential(t, (*recorded)[0], providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    decompositionBacklogOwner,
		Name:     decompositionBacklogName,
	})

	data, err := os.ReadFile(filepath.Join(workDir, "plan-validation.json"))
	if err != nil {
		t.Fatalf("read plan-validation.json: %v", err)
	}
	var got validatePlanResult
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Valid || got.Conflict {
		t.Fatalf("plan-validation = %+v, stdout = %q; want a valid, conflict-free read of the backlog parent", got, stdout)
	}
}

// TestPublishBatchBindsPublisherToBacklogRepository asserts publish-batch's
// publisher is anchored on the backlog container. decomposition.Publisher
// refuses a plan whose parent names a different repository than its own, so
// the binding is observable without a full publication: a plan parented in the
// PROJECT repo is now rejected, and one parented in the backlog is accepted
// past that gate. The provider is also built for the backlog with the backlog
// credential (before this, the per-provider switch hard-coded the routed repo
// and the project's capability token).
func TestPublishBatchBindsPublisherToBacklogRepository(t *testing.T) {
	root := t.TempDir()
	crossRepositoryDecompositionEnv(t, root, "decomposition-run-1", 905, providers.LabelApproved)
	workDir := t.TempDir()
	t.Chdir(workDir)

	backlogRepository := decompositionBacklogOwner + "/" + decompositionBacklogName
	projectRepository := decompositionBacklogOwner + "/" + decompositionProjectName

	writePublishInputs := func(t *testing.T, repository string) {
		t.Helper()
		plan := validDecompositionPlan(decomposition.Selection{
			Mode:                decomposition.SelectionModeEscalation,
			SourceRunID:         "escalated-publish",
			IssueSnapshotDigest: "sha256:fixture",
			Parent: decomposition.ParentRef{
				Provider: "github", Repository: repository, ID: "905", ObservedRevision: "1",
			},
		})
		digest, err := decomposition.PlanDigest(plan)
		if err != nil {
			t.Fatalf("digest plan: %v", err)
		}
		for name, value := range map[string]any{
			"plan.json":            plan,
			"plan-validation.json": validatePlanResult{Valid: true, PlanDigest: digest},
		} {
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatalf("marshal %s: %v", name, err)
			}
			if err := os.WriteFile(filepath.Join(workDir, name), data, 0o644); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
		}
	}

	const bindingError = "does not match publisher repository"

	t.Run("project-parented plan is refused", func(t *testing.T) {
		writePublishInputs(t, projectRepository)
		code, _, stderr := runArgs(t, "publish-batch", root)
		if code == 0 || !strings.Contains(stderr, bindingError) {
			t.Fatalf("code = %d, stderr = %q; want the publisher bound to the backlog to refuse a project-parented plan", code, stderr)
		}
		if !strings.Contains(stderr, backlogRepository) {
			t.Fatalf("stderr = %q, want the backlog repository named as the publisher's", stderr)
		}
	})

	t.Run("backlog-parented plan passes the binding gate", func(t *testing.T) {
		writePublishInputs(t, backlogRepository)
		recorded := recordStageProviderConfigs(t)
		_, _, stderr := runArgs(t, "publish-batch", root)
		if strings.Contains(stderr, bindingError) {
			t.Fatalf("stderr = %q; a backlog-parented plan must pass the publisher binding gate", stderr)
		}
		if len(*recorded) != 1 {
			t.Fatalf("recorded %d provider builds, want 1", len(*recorded))
		}
		assertBacklogCredential(t, (*recorded)[0], providers.RepositoryRef{
			Provider: providers.ProviderGitHub,
			Owner:    decompositionBacklogOwner,
			Name:     decompositionBacklogName,
		})
	})
}

// TestPublishBatchReleaseKeyMatchesBacklogScopedParentClaim locks the release
// half of the finding. publish-batch used to rebuild the parent key from the
// routed repository's gaggle scope; ClaimLedger.release is idempotent by
// design, so that key silently no-op'd against select-source's backlog-scoped
// lease and left the parent claimed until the lease expired — blocking every
// later run on that item for a full lease period after a successful
// publication.
func TestPublishBatchReleaseKeyMatchesBacklogScopedParentClaim(t *testing.T) {
	root := t.TempDir()
	schedulerDir := filepath.Join(root, "scheduler")
	instanceLog, _, err := journal.OpenInstanceLog(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = instanceLog.Close() }()

	identity := decompositionBacklogIdentity()
	backlogRepo := providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    decompositionBacklogOwner,
		Name:     decompositionBacklogName,
	}
	const (
		runID  = "decomposition-run-1"
		gaggle = "decomposer"
		itemID = "906"
	)
	key := selectSourceClaimKey(identity, gaggle, backlogRepo, itemID)
	if backlog := key.Backlog; backlog.IsZero() || !backlog.Equal(identity) {
		t.Fatalf("claim key backlog = %+v, want the resolved backlog identity", backlog)
	}
	if ok, _, err := claimSelectSourceParent(schedulerDir, instanceLog, key, runID, "decomposition", time.Hour); err != nil || !ok {
		t.Fatalf("claim parent: ok=%v err=%v", ok, err)
	}

	// The pre-fix key: same gaggle, same item, no backlog identity.
	legacyKey := localscheduler.ClaimKey{Gaggle: gaggle, Provider: "github", ExternalID: itemID}
	if err := releaseSelectSourceParent(schedulerDir, instanceLog, legacyKey, runID); err != nil {
		t.Fatalf("release with the gaggle-scoped key: %v", err)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(schedulerDir, claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, held := ledger.LookupScoped(key); !held {
		t.Fatal("fixture is vacuous: the gaggle-scoped key released the backlog-scoped lease")
	}

	if err := releaseSelectSourceParent(schedulerDir, instanceLog, key, runID); err != nil {
		t.Fatalf("release with the backlog-scoped key: %v", err)
	}
	ledger, err = localscheduler.OpenClaimLedger(filepath.Join(schedulerDir, claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if entry, held := ledger.LookupScoped(key); held {
		t.Fatalf("parent lease survived publication release: %+v", entry)
	}
}

// TestDecompositionStagesFallBackToProjectRepositoryWithoutBacklog preserves
// the same-project/same-backlog majority: no connection is requested and the
// stages keep addressing the routed repository with its capability token.
func TestDecompositionStagesFallBackToProjectRepositoryWithoutBacklog(t *testing.T) {
	root := t.TempDir()
	buildSelectSourceRun(t, root, selectSourceRunOptions{
		runID:          "escalated-same-repo",
		startedAt:      time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
		claimedIssueID: "907",
		claimProvider:  "github",
		finalPhase:     journal.PhaseEscalated,
		events:         nonRetryableEscalationEvents("ISSUE_OVER_SCOPE", "too large"),
	})
	server := newFakeGitHubServer(t, "acme", "widgets")
	server.addIssue(907, "A very large issue", providers.LabelApproved)
	providerCmdEnv(t, server, executor.CredentialEnvVar(string(capability.GitHubIssuesWrite)), "decomposition-run-1")
	t.Setenv(executor.BacklogProviderEnvVar, "")
	t.Setenv(executor.BacklogConnectionRefEnvVar, "")
	t.Setenv("GOOBERS_GAGGLE", "")
	decompositionInstanceEnv(t, root)
	recorded := recordStageProviderConfigs(t)

	workDir := t.TempDir()
	t.Chdir(workDir)
	if code, _, stderr := runArgs(t, "select-source", root); code != 0 {
		t.Fatalf("select-source: code = %d, stderr = %q", code, stderr)
	}
	if len(*recorded) != 1 {
		t.Fatalf("recorded %d provider builds, want 1", len(*recorded))
	}
	cfg := (*recorded)[0]
	if cfg.connectionRef != "" {
		t.Fatalf("connectionRef = %q, want none when no backlog connection is declared", cfg.connectionRef)
	}
	if cfg.repo.Owner != "acme" || cfg.repo.Name != "widgets" {
		t.Fatalf("provider built for %s/%s, want the routed repository", cfg.repo.Owner, cfg.repo.Name)
	}
	if selection := readSelection(t, workDir); selection.Parent.Repository != "acme/widgets" {
		t.Fatalf("selection parent repository = %q, want the routed repository", selection.Parent.Repository)
	}
}

// TestClosedPRReconciliationSplitsBacklogAndCodeRepositories covers the
// adjacent backlog-item path the same finding left behind inside backlog-query
// itself: the in-review requeue reads and relabels WORK ITEMS (backlog
// container) while reading PULL REQUESTS and matching breadcrumb URLs against
// the routed CODE repository. Addressing both halves at one repository is only
// correct in the same-repository majority; on a split topology the work-item
// listing hit the code repo and the requeue silently never fired.
func TestClosedPRReconciliationSplitsBacklogAndCodeRepositories(t *testing.T) {
	backlogServer := newFakeGitHubServer(t, decompositionBacklogOwner, decompositionBacklogName)
	backlogServer.addIssue(7, "Implement safely", providers.LabelApproved, providers.LabelReady, inReviewStatusLabel)
	codeServer := newFakeGitHubServer(t, decompositionBacklogOwner, decompositionProjectName)
	codeServer.addOpenPR(101, "goobers/implementation/run-1", "main", "head", "base", false, nil, nil)
	codeServer.setPRBody(101, "## Summary\n\n---\nFixes #7\n\n---\ngoobers run-id: run-1")
	codeServer.setPRClosed(101)
	// The breadcrumb comment lives on the backlog item but links a PR in the
	// code repository.
	backlogServer.addComment(7, implementationInReviewComment(
		"https://github.com/"+decompositionBacklogOwner+"/"+decompositionProjectName+"/pull/101"))

	recorder := &requeueMutationRecorder{}
	issueProvider := backlogServer.newGitHubProvider("backlog-token", providers.WithMutationRecorder(recorder))
	prProvider := codeServer.newGitHubProvider("project-token")
	codeRepo := providers.RepositoryRef{
		Provider: providers.ProviderGitHub, Owner: decompositionBacklogOwner, Name: decompositionProjectName,
	}
	backlogRepo := providers.RepositoryRef{
		Provider: providers.ProviderGitHub, Owner: decompositionBacklogOwner, Name: decompositionBacklogName,
	}

	if err := reconcileClosedUnmergedInReview(
		context.Background(), issueProvider, prProvider, codeRepo, backlogRepo,
	); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	backlogServer.mu.Lock()
	labels := append([]string(nil), backlogServer.issues[7].labels...)
	backlogServer.mu.Unlock()
	if hasAllLabels(labels, []string{inReviewStatusLabel}) {
		t.Fatalf("backlog item labels = %v, want %q removed after its linked PR closed unmerged", labels, inReviewStatusLabel)
	}
	if got := recorder.count(); got != 1 {
		t.Fatalf("mutation count = %d, want exactly one requeue on the backlog item", got)
	}
}
