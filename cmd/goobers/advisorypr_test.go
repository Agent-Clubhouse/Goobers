package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/stateclient"
	"github.com/goobers/goobers/providers"
)

func advisoryFixture(t *testing.T) (*fakeGitHubServer, string) {
	t.Helper()
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	root := initDemo(t)
	providerCmdEnv(t, server, executor.CredentialEnvVar("github:pr:read"), "advisory-run-1")
	t.Setenv(executor.CredentialEnvVar("github:pr:write"), "test-token")
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	t.Setenv("GOOBERS_WORKFLOW", "advisory-pr-architecture")
	t.Setenv(executor.InputEnvVar("reviewType"), "architecture")
	t.Setenv(executor.InputEnvVar("expectedAuthor"), server.authenticatedLogin)
	t.Chdir(t.TempDir())
	return server, root
}

func TestAdvisoryPRSelectRefusesNonGitHubProviders(t *testing.T) {
	for _, kind := range []providers.ProviderKind{providers.ProviderADO, providers.ProviderGitea} {
		t.Run(string(kind), func(t *testing.T) {
			root := initDemo(t)
			setNonGitHubStageEnv(t, kind)
			t.Setenv(executor.RunIDEnvVar, "advisory-dispatch-probe")
			t.Setenv(executor.WorkflowEnvVar, "advisory-pr-architecture")
			t.Setenv(executor.InputEnvVar("reviewType"), "architecture")
			code, _, stderr := runArgs(t, "advisory-pr-select", root)
			if code != 1 || !strings.Contains(stderr, "advisory PR review currently requires GitHub") {
				t.Fatalf("%s select: code=%d stderr=%q", kind, code, stderr)
			}
		})
	}
}

func TestAdvisoryPRPublishRefusesNonGitHubProviders(t *testing.T) {
	for _, kind := range []providers.ProviderKind{providers.ProviderADO, providers.ProviderGitea} {
		t.Run(string(kind), func(t *testing.T) {
			root := initDemo(t)
			setNonGitHubStageEnv(t, kind)
			t.Setenv(executor.RunIDEnvVar, "advisory-dispatch-probe")
			t.Setenv(executor.WorkflowEnvVar, "advisory-pr-architecture")
			t.Setenv(executor.InputEnvVar("reviewType"), "architecture")
			code, _, stderr := runArgs(t, "advisory-pr-publish", root)
			if code != 1 || !strings.Contains(stderr, "advisory PR review currently requires GitHub") {
				t.Fatalf("%s publish: code=%d stderr=%q", kind, code, stderr)
			}
		})
	}
}

func advisoryPR(t *testing.T, server *fakeGitHubServer, number int, draft bool) string {
	t.Helper()
	sha := strings.Repeat(string(rune('a'+number%20)), 40)
	server.addIssue(number, "PR issue")
	server.addOpenPR(number, "feature/test", "main", sha, strings.Repeat("b", 40), draft, nil,
		[]fakePRFile{{path: "api/v1alpha1/workflow.go", status: "modified", patch: "@@ -1 +1 @@\n-old\n+new"}})
	return sha
}

func selectedAdvisory(t *testing.T, root string) advisorySelection {
	t.Helper()
	code, _, stderr := runArgs(t, "advisory-pr-select", root)
	if code != 0 {
		t.Fatalf("select: code=%d stderr=%s", code, stderr)
	}
	data, err := os.ReadFile(advisorySelectionFile)
	if err != nil {
		t.Fatal(err)
	}
	var selection advisorySelection
	if err := json.Unmarshal(data, &selection); err != nil {
		t.Fatal(err)
	}
	return selection
}

func reviewAdvisory(t *testing.T, root string, selection advisorySelection, decision, comment string) {
	t.Helper()
	var number int
	if _, err := fmt.Sscanf(selection.SelectedNumber, "%d", &number); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(advisoryReview{
		Schema: advisorySchema, ReviewType: selection.ReviewType, Number: number,
		HeadSHA: selection.SelectedHeadSHA, Decision: decision, Comment: comment,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(advisoryReviewFile, data, 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runArgs(t, "advisory-pr-publish", root)
	if code != 0 {
		t.Fatalf("publish: code=%d stderr=%s", code, stderr)
	}
}

func TestAdvisorySelectionIncludesOldHumanBotAndDraftAndSkipPersists(t *testing.T) {
	server, root := advisoryFixture(t)
	sha1 := advisoryPR(t, server, 1, false)
	advisoryPR(t, server, 2, true)
	advisoryPR(t, server, 3, false)
	server.setPRIdentities(1, "human", nil, nil)
	server.setPRIdentities(2, "draft-bot[bot]", nil, nil)
	server.setPRIdentities(3, "other-human", nil, nil)
	server.setPRCheckState(1, "failure")
	first := selectedAdvisory(t, root)
	if first.SelectedNumber != "1" || first.SelectedHeadSHA != sha1 || len(first.Files) != 1 {
		t.Fatalf("first selection = %+v", first)
	}
	reviewAdvisory(t, root, first, "skip", "")
	server.mu.Lock()
	comments := len(server.issues[1].comments)
	server.mu.Unlock()
	if comments != 0 {
		t.Fatalf("private skip posted %d comments", comments)
	}
	server.setPRHead(1, strings.Repeat("f", 40), nil)
	t.Setenv(executor.RunIDEnvVar, "advisory-run-2")
	second := selectedAdvisory(t, root)
	if second.SelectedNumber != "2" || !second.Draft {
		t.Fatalf("second selection = %+v; want draft PR #2", second)
	}
	t.Setenv(executor.InputEnvVar("reviewType"), "security")
	t.Setenv(executor.RunIDEnvVar, "advisory-run-3")
	third := selectedAdvisory(t, root)
	if third.SelectedNumber != "1" {
		t.Fatalf("independent review type selected #%s; want #1", third.SelectedNumber)
	}
	t.Setenv(executor.RunIDEnvVar, "")
	code, _, stderr := runArgs(t, "advisory-pr-reset", "--gaggle", "goobers", "--owner", server.owner,
		"--repo", server.repo, "--review-type", "architecture", "--pr", "1", root)
	if code != 0 {
		t.Fatalf("operator reset: code=%d stderr=%s", code, stderr)
	}
	ledger, err := openStageClaimLedger(instance.NewLayout(root))
	if err != nil {
		t.Fatal(err)
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: server.owner, Name: server.repo}
	if err := ledger.ReleaseScoped(t.Context(), advisoryClaimKey(repo, "architecture", 1), "advisory-run-1"); err != nil {
		t.Fatal(err)
	}
	t.Setenv(executor.InputEnvVar("reviewType"), "architecture")
	t.Setenv(executor.RunIDEnvVar, "advisory-run-4")
	fourth := selectedAdvisory(t, root)
	if fourth.SelectedNumber != "1" {
		t.Fatalf("reset PR was not selected: %+v", fourth)
	}
}

func TestAdvisoryInterestingCommentIsIdempotentAfterReceiptLoss(t *testing.T) {
	server, root := advisoryFixture(t)
	advisoryPR(t, server, 4, false)
	selection := selectedAdvisory(t, root)
	comment := "The new workflow field weakens the declarative contract because its meaning depends on runtime shell state. Could this be an explicit typed input?"
	reviewAdvisory(t, root, selection, "interesting", comment)
	server.mu.Lock()
	comments := append([]string(nil), server.issues[4].comments...)
	server.mu.Unlock()
	if len(comments) != 1 || !strings.Contains(comments[0], "<!-- goobers-advisory:architecture:") {
		t.Fatalf("comments = %v", comments)
	}
	// Drop the private receipt as if the provider committed but the state
	// write failed. The stable public marker prevents a second comment.
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: server.owner, Name: server.repo}
	store, err := advisoryStore(root)
	if err != nil {
		t.Fatal(err)
	}
	key := advisoryKey(repo, "architecture", 4)
	if err := store.Update(t.Context(), key, "test.drop-receipt", func(stateclient.Value) ([]byte, bool, error) {
		return []byte(`{}`), true, nil
	}); err != nil {
		t.Fatal(err)
	}
	reviewAdvisory(t, root, selection, "interesting", comment)
	server.mu.Lock()
	count := len(server.issues[4].comments)
	server.mu.Unlock()
	if count != 1 {
		t.Fatalf("retry posted %d comments", count)
	}
}

func TestAdvisoryMalformedReviewIsNeverSkip(t *testing.T) {
	for _, body := range []string{
		`{"schema":"goobers.dev/advisory-pr-review/v1","reviewType":"architecture","number":1,"headSha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","decision":"skip","comment":"oops"}`,
		`{"schema":"goobers.dev/advisory-pr-review/v1","reviewType":"architecture","number":1,"headSha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","decision":"skip","comment":"","extra":true}`,
	} {
		if _, err := strictAdvisoryReview([]byte(body)); err == nil {
			t.Fatalf("accepted malformed review %s", body)
		}
	}
}

func TestAdvisoryConcurrentSelectionAndStaleHeadRemainPrivate(t *testing.T) {
	server, root := advisoryFixture(t)
	advisoryPR(t, server, 5, false)
	advisoryPR(t, server, 6, false)
	first := selectedAdvisory(t, root)
	if first.SelectedNumber != "5" {
		t.Fatalf("first selection = %+v", first)
	}
	t.Setenv(executor.RunIDEnvVar, "advisory-run-2")
	second := selectedAdvisory(t, root)
	if second.SelectedNumber != "6" {
		t.Fatalf("concurrent selection = %+v; want #6", second)
	}
	// Return to the first run and advance its head before publishing. The
	// strict reviewer artifact remains valid but is now stale.
	t.Setenv(executor.RunIDEnvVar, "advisory-run-1")
	data, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(advisorySelectionFile, data, 0o644); err != nil {
		t.Fatal(err)
	}
	server.setPRHead(5, strings.Repeat("e", 40), nil)
	reviewAdvisory(t, root, first, "interesting", "The new DSL branch changes the meaning of a declared transition.")
	server.mu.Lock()
	comments := len(server.issues[5].comments)
	server.mu.Unlock()
	if comments != 0 {
		t.Fatalf("stale head posted %d comments", comments)
	}
	result, err := os.ReadFile(advisoryResultFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result), `"stale-or-closed"`) {
		t.Fatalf("result = %s", result)
	}
	ledger, err := openStageClaimLedger(instance.NewLayout(root))
	if err != nil {
		t.Fatal(err)
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: server.owner, Name: server.repo}
	if err := ledger.ReleaseScoped(t.Context(), advisoryClaimKey(repo, "architecture", 5), "advisory-run-1"); err != nil {
		t.Fatal(err)
	}
	// A later run can select the new head because stale findings did not
	// become either a skip or a reviewed-head receipt.
	t.Setenv(executor.RunIDEnvVar, "advisory-run-3")
	third := selectedAdvisory(t, root)
	if third.SelectedNumber != "5" || third.SelectedHeadSHA != strings.Repeat("e", 40) {
		t.Fatalf("new-head selection = %+v", third)
	}
}

func TestAdvisoryPublisherRequiresBotIdentityAndIgnoresSpoofedMarker(t *testing.T) {
	server, root := advisoryFixture(t)
	advisoryPR(t, server, 7, false)
	selection := selectedAdvisory(t, root)
	comment := "This DSL field creates a second source of truth for transitions; could the existing declaration carry it?"
	sum := sha256.Sum256([]byte(comment))
	marker := "<!-- goobers-advisory:architecture:" + selection.SelectedHeadSHA + ":" + hex.EncodeToString(sum[:]) + " -->"
	server.addRawCommentAs(7, "untrusted-user", marker)
	t.Setenv(executor.InputEnvVar("expectedAuthor"), "wrong-bot[bot]")
	data, err := json.Marshal(advisoryReview{Schema: advisorySchema, ReviewType: "architecture", Number: 7,
		HeadSHA: selection.SelectedHeadSHA, Decision: "interesting", Comment: comment})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(advisoryReviewFile, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runArgs(t, "advisory-pr-publish", root); code == 0 {
		t.Fatal("publisher accepted a different identity")
	}
	server.mu.Lock()
	count := len(server.issues[7].comments)
	server.mu.Unlock()
	if count != 1 {
		t.Fatalf("wrong identity added %d comments", count-1)
	}
	t.Setenv(executor.InputEnvVar("expectedAuthor"), server.authenticatedLogin)
	reviewAdvisory(t, root, selection, "interesting", comment)
	server.mu.Lock()
	count = len(server.issues[7].comments)
	server.mu.Unlock()
	if count != 2 {
		t.Fatalf("spoofed marker suppressed bot comment: count=%d", count)
	}
}
