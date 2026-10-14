package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
)

// #6494: rebase-pr classifies comments with the classifier pr-comment-watch
// routes on, so the two can never disagree about who is human.

func TestHumanCommentNeedsRemediationFollowsIdentityMode(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	own := func(body string) providers.Comment {
		return providers.Comment{ID: "own", Author: "goobers-bot", Body: body, CreatedAt: &at}
	}
	marked := stampOwnFixtureBody("Addressed in the latest push.", "comment")
	login := loginCommentIdentity("goobers-bot")
	shared := commentIdentitySettings{mode: commentIdentityShared}
	dedicated := commentIdentitySettings{mode: commentIdentityDedicated}

	tests := []struct {
		name     string
		self     commentIdentity
		settings commentIdentitySettings
		comment  providers.Comment
		want     bool
	}{
		{"shared: unmarked comment by the credential's own login is a person's", login, shared, own("please add a README note"), true},
		{"dedicated: unmarked comment by the credential's own login is Goobers'", login, dedicated, own("please add a README note"), false},
		{"shared: a marked comment is Goobers' own", login, shared, own(marked), false},
		{"dedicated: a marked comment is Goobers' own", login, dedicated, own(marked), false},
		{"shared: a distinct person is human", login, shared, providers.Comment{Author: "alice", Body: "nit", CreatedAt: &at}, true},
		{"dedicated: a distinct person is human", login, dedicated, providers.Comment{Author: "alice", Body: "nit", CreatedAt: &at}, true},
		{"a Bot-typed author is automation in either mode", login, shared, providers.Comment{Author: "ci[bot]", AuthorType: "Bot", Body: "coverage", CreatedAt: &at}, false},
		{"excludeAuthors names automation", login, commentIdentitySettings{mode: commentIdentityShared, excludeAuthors: map[string]bool{"ci-runner": true}}, providers.Comment{Author: "ci-runner", Body: "coverage", CreatedAt: &at}, false},
		{
			"ADO shared: unmarked comment by the credential's identity id is a person's",
			adoCommentIdentity(providers.ADOIdentity{ID: "self-guid", DisplayName: "Operator"}), shared,
			providers.Comment{Author: "Operator", AuthorID: "self-guid", Body: "please add a README note", CreatedAt: &at}, true,
		},
		{
			"ADO dedicated: unmarked comment by the credential's identity id is Goobers'",
			adoCommentIdentity(providers.ADOIdentity{ID: "self-guid", DisplayName: "Operator"}), dedicated,
			providers.Comment{Author: "Operator", AuthorID: "self-guid", Body: "please add a README note", CreatedAt: &at}, false,
		},
		{
			"ADO dedicated: a display-name twin with another id is a person",
			adoCommentIdentity(providers.ADOIdentity{ID: "self-guid", DisplayName: "Operator"}), dedicated,
			providers.Comment{Author: "Operator", AuthorID: "other-guid", Body: "please add a README note", CreatedAt: &at}, true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			classifier := newCommentClassifier(test.self, test.settings)
			if got := humanCommentNeedsRemediation([]providers.Comment{test.comment}, classifier); got != test.want {
				t.Fatalf("humanCommentNeedsRemediation = %v, want %v", got, test.want)
			}
			// The rebase-pr cause and pr-comment-watch's routing rule agree.
			if _, fresh, _ := latestUnaddressedHumanComment([]providers.Comment{test.comment}, classifier); fresh != test.want {
				t.Fatalf("latestUnaddressedHumanComment fresh = %v, want %v (pr-comment-watch would disagree)", fresh, test.want)
			}
		})
	}
}

// A checkpoint's watermark says only that the comment was seen. A human
// comment no Goobers response has acknowledged still needs remediation, or a
// rebase-only pass would clear the label pr-comment-watch set for it.
func TestHumanCommentNeedsRemediationUnansweredBehindWatermark(t *testing.T) {
	stickyAt := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	humanAt := stickyAt.Add(time.Hour)
	watermark := humanAt.Add(time.Hour)
	state, err := remediationStateComment(remediationState{Cycles: 1, LastSeenCommentAt: watermark.Format(time.RFC3339)})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}
	sticky := providers.Comment{ID: "state", Author: "goobers-bot", Body: stampOwnFixtureBody(state, "comment"), CreatedAt: &stickyAt}
	human := providers.Comment{ID: "h", Author: "alice", Body: "please add a README note", CreatedAt: &humanAt}
	classifier := dedicatedLoginClassifier("goobers-bot")

	if hasNewHumanCommentSince([]providers.Comment{sticky, human}, classifier) {
		t.Fatal("watermark arm fired for a comment at or before the watermark; the test no longer isolates the acknowledgement arm")
	}
	if !humanCommentNeedsRemediation([]providers.Comment{sticky, human}, classifier) {
		t.Fatal("an unanswered human comment behind the watermark did not need remediation")
	}

	answeredAt := watermark.Add(time.Minute)
	answer := providers.Comment{
		ID: "r", Author: "goobers-bot", CreatedAt: &answeredAt,
		Body: stampOwnFixtureBody(withFeedbackAck("Added the note.", feedbackAckMarker(humanAt)), "comment"),
	}
	if humanCommentNeedsRemediation([]providers.Comment{sticky, human, answer}, classifier) {
		t.Fatal("a human comment answered by a response whose frontier covers it still needed remediation")
	}
}

// runRebasePRWithOwnLoginComment runs rebase-pr on GitHub with a clean rebase,
// no finding, green CI and a needs-remediation label, where the PR's only
// comment after Goobers' sticky state is an unmarked one by the credential's
// own login.
func runRebasePRWithOwnLoginComment(t *testing.T, prNumber, identityMode string) (result string, labels []string) {
	t.Helper()
	prBranch := "goobers/impl/run-6494-" + prNumber
	origin := initNonConflictingPRBranch(t, prBranch)
	wt := prWorktree(t, origin, prBranch)

	watermark := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	priorState, err := remediationStateComment(remediationState{Cycles: 1, LastDiffDigest: "sha256:prior", LastSeenCommentAt: watermark.Format(time.RFC3339)})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}
	st := &rebasePRServerState{
		labels:           []string{needsRemediationLabel},
		comments:         []string{priorState, "Please add a short note to the README describing this change."},
		commentAuthors:   []string{"goobers-bot", "goobers-bot"},
		commentCreatedAt: []string{watermark.Format(time.RFC3339), watermark.Add(time.Hour).Format(time.RFC3339)},
		commentUnmarked:  []bool{false, true},
	}
	number, err := strconv.Atoi(prNumber)
	if err != nil {
		t.Fatalf("prNumber: %v", err)
	}
	server := st.start(t, "your-org", "your-repo", number)
	inputs := map[string]string{
		"selectedNumber": prNumber, "head": prBranch, "base": "main",
		"hasSubstantiveFindings": "false", "hasFailingCI": "false", "remediate": defaultRemediatePolicy,
	}
	if identityMode != "" {
		inputs["identityMode"] = identityMode
	}
	instanceRoot := rebasePREnv(t, server.URL, wt.Path, inputs)

	code, stdout, stderr := runArgs(t, "rebase-pr", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(wt.Path, "rebase-result.json"))
	if err != nil {
		t.Fatalf("read rebase-result.json: %v", err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return string(data), append([]string(nil), st.labels...)
}

func TestRebasePRSharedIdentityKeepsOwnLoginHumanComment(t *testing.T) {
	result, labels := runRebasePRWithOwnLoginComment(t, "6494", commentIdentityShared)
	if !strings.Contains(result, `"needsAgent":"true"`) || !strings.Contains(result, `"remediationCauses":"human-comment"`) {
		t.Fatalf("rebase-result.json = %s, want needsAgent=true cause=human-comment under identityMode=shared", result)
	}
	if len(labels) != 1 || labels[0] != needsRemediationLabel {
		t.Fatalf("labels = %v, want %s kept for the unanswered comment", labels, needsRemediationLabel)
	}
}

func TestRebasePRDedicatedIdentityTreatsOwnLoginCommentAsGoobers(t *testing.T) {
	result, labels := runRebasePRWithOwnLoginComment(t, "6495", "")
	if !strings.Contains(result, `"needsAgent":"false"`) {
		t.Fatalf("rebase-result.json = %s, want needsAgent=false: on GitHub the default dedicated mode reads the own-login comment as Goobers'", result)
	}
	if len(labels) != 0 {
		t.Fatalf("labels = %v, want %s cleared on a clean rebase with no human feedback", labels, needsRemediationLabel)
	}
}

func TestRebasePRRejectsInvalidIdentityMode(t *testing.T) {
	const prBranch = "goobers/impl/run-6494-invalid"
	origin := initNonConflictingPRBranch(t, prBranch)
	wt := prWorktree(t, origin, prBranch)
	st := &rebasePRServerState{labels: []string{needsRemediationLabel}}
	server := st.start(t, "your-org", "your-repo", 6496)
	instanceRoot := rebasePREnv(t, server.URL, wt.Path, map[string]string{
		"selectedNumber": "6496", "head": prBranch, "base": "main",
		"hasSubstantiveFindings": "false", "hasFailingCI": "false", "remediate": defaultRemediatePolicy,
		"identityMode": "sometimes",
	})
	code, stdout, stderr := runArgs(t, "rebase-pr", instanceRoot)
	if code == 0 || !strings.Contains(stderr, `invalid identityMode "sometimes"`) {
		t.Fatalf("code = %d, stdout = %q, stderr = %q, want a failure naming the invalid identityMode", code, stdout, stderr)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.labels) != 1 {
		t.Fatalf("labels = %v, want %s untouched when the stage cannot classify comments", st.labels, needsRemediationLabel)
	}
}

// runADORebasePRWithHumanComment runs rebase-pr on Azure DevOps with a clean
// rebase, no finding and a needs-remediation label, where a person left one
// active-thread comment authored by authorID with no Goobers marker.
func runADORebasePRWithHumanComment(t *testing.T, prNumber int, authorID, identityMode string) (result string, labels []string) {
	t.Helper()
	root, repo := providerDispatchFixture(t, providers.ProviderADO)
	prBranch := "goobers/impl/ado-6494-" + authorID + "-" + identityMode
	origin := initNonConflictingPRBranch(t, prBranch)
	wt := prWorktree(t, origin, prBranch)

	st := &adoRemediationServerState{
		owner: repo.Owner, project: repo.Project, name: repo.Name,
		prNumber: prNumber, prBranch: prBranch, base: "main",
		headSHA: "unused-head", baseSHA: "unused-base", status: "active",
		labels: []string{needsRemediationLabel},
		humanComments: []adoFixtureHumanComment{{
			body: "Please add a short note to the README describing this change.", authorID: authorID,
			publishedDate: "2026-10-01T12:00:00Z",
		}},
	}
	server := st.start(t)
	installADOStageProvider(t, repo, server)

	t.Setenv("GOOBERS_RUN_ID", "run-6494-ado")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	setDeliveredADOStageCredentials(t)
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", strconv.Itoa(prNumber))
	t.Setenv("GOOBERS_INPUT_HEAD", prBranch)
	t.Setenv("GOOBERS_INPUT_BASE", "main")
	t.Setenv("GOOBERS_INPUT_HASSUBSTANTIVEFINDINGS", "false")
	if identityMode != "" {
		t.Setenv("GOOBERS_INPUT_IDENTITYMODE", identityMode)
	}
	t.Chdir(wt.Path)

	code, stdout, stderr := runArgs(t, "rebase-pr", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(wt.Path, "rebase-result.json"))
	if err != nil {
		t.Fatalf("read rebase-result.json: %v", err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return string(data), append([]string(nil), st.labels...)
}

func TestRebasePRADOSharedIdentityKeepsOwnIdentityHumanComment(t *testing.T) {
	// identityMode left unset: shared is the Azure DevOps default.
	result, labels := runADORebasePRWithHumanComment(t, 64, adoRemediationSelfID, "")
	if !strings.Contains(result, `"needsAgent":"true"`) || !strings.Contains(result, `"remediationCauses":"human-comment"`) {
		t.Fatalf("rebase-result.json = %s, want needsAgent=true cause=human-comment", result)
	}
	if len(labels) != 1 || labels[0] != needsRemediationLabel {
		t.Fatalf("labels = %v, want %s kept for the unanswered comment", labels, needsRemediationLabel)
	}
}

func TestRebasePRADODedicatedIdentityTreatsOwnIdentityCommentAsGoobers(t *testing.T) {
	result, labels := runADORebasePRWithHumanComment(t, 65, adoRemediationSelfID, commentIdentityDedicated)
	if !strings.Contains(result, `"needsAgent":"false"`) {
		t.Fatalf("rebase-result.json = %s, want needsAgent=false under identityMode=dedicated", result)
	}
	if len(labels) != 0 {
		t.Fatalf("labels = %v, want %s cleared", labels, needsRemediationLabel)
	}
}

func TestRebasePRADODistinctIdentityHumanCommentInEitherMode(t *testing.T) {
	for i, mode := range []string{commentIdentityShared, commentIdentityDedicated} {
		t.Run(mode, func(t *testing.T) {
			result, labels := runADORebasePRWithHumanComment(t, 66+i, "reviewer-guid", mode)
			if !strings.Contains(result, `"remediationCauses":"human-comment"`) {
				t.Fatalf("rebase-result.json = %s, want cause=human-comment", result)
			}
			if len(labels) != 1 {
				t.Fatalf("labels = %v, want %s kept", labels, needsRemediationLabel)
			}
		})
	}
}
