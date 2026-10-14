package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/findingledger"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// #5890: a needs-changes verdict raised a substantive finding on a pull
// request's head and base. A later pass on the SAME head and base never
// mentioned it. Nothing about the finding was resolved, so neither a healer,
// apply-verdict, nor merge-pr may treat the later pass as clearing it. Only an
// explicit disposition (resolved with evidence, rejected with a reason) does.

const unboundedLineageID = "unbounded-lineage-lookup"

func unboundedLineageFinding() apiv1.Finding {
	return apiv1.Finding{
		ID:       unboundedLineageID,
		Severity: apiv1.SeverityError,
		Class:    apiv1.FindingSubstantive,
		Message:  "Lineage lookup scans unrelated journals with unbounded ancestry and continuation fan-out.",
		Location: "internal/readservice/runs.go:3181-3202",
	}
}

func rejectUnboundedLineage() []apiv1.FindingDisposition {
	return []apiv1.FindingDisposition{{
		FindingID: unboundedLineageID,
		Outcome:   apiv1.FindingRejected,
		Reason:    "The scan is bounded by the caller's page size; see the guard at runs.go:3170.",
	}}
}

type recordingStageAnnotator struct{ events *[]journal.Event }

func (r recordingStageAnnotator) Append(event journal.Event) error {
	*r.events = append(*r.events, event)
	return nil
}

func (recordingStageAnnotator) Close() error { return nil }

// captureMergeSafetyBlocks records the merge-safety-block annotations the
// stage under test journals.
func captureMergeSafetyBlocks(t *testing.T) func() []map[string]any {
	t.Helper()
	var events []journal.Event
	previous := openStageAnnotator
	openStageAnnotator = func(instance.Layout) (stageAnnotator, error) {
		return recordingStageAnnotator{events: &events}, nil
	}
	t.Cleanup(func() { openStageAnnotator = previous })
	return func() []map[string]any {
		var blocks []map[string]any
		for _, event := range events {
			if event.Type == journal.EventRunnerAnnotation && event.Runner["annotation"] == "merge-safety-block" {
				blocks = append(blocks, event.Runner)
			}
		}
		return blocks
	}
}

func verdictCommentFixture(id int, author string, v apiv1.Verdict) map[string]interface{} {
	return map[string]interface{}{
		"id": id, "user": map[string]string{"login": author}, "created_at": "2026-09-26T16:50:28Z",
		"body": renderVerdictComment(v),
	}
}

func TestGatherPRVerdictCarriesUnansweredSameHeadFinding(t *testing.T) {
	const head, base = "head-sha", "base-sha"
	rejection := renderVerdictComment(apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "Unbounded lineage lookup.", HeadSHA: head, BaseSHA: base, Findings: []apiv1.Finding{unboundedLineageFinding()}})
	generic := apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "Trace text is fixed.", HeadSHA: head, BaseSHA: base}
	answered := generic
	answered.Dispositions = rejectUnboundedLineage()
	movedBase := generic
	movedBase.BaseSHA = "newer-base-sha"
	movedHead := generic
	movedHead.HeadSHA = "newer-head-sha"
	forged, err := verdictJSONComment(answered)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		sticky   apiv1.Verdict
		wantPass bool
	}{
		{name: "generic later pass", sticky: generic},
		{name: "explicit rejection", sticky: answered, wantPass: true},
		// A base-branch advance does not change the PR's own diff.
		{name: "generic pass after the base moved", sticky: movedBase},
		{name: "pass on a new head is a new review subject", sticky: movedHead, wantPass: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			comments := []providers.Comment{
				// The canonical status comment is the oldest marked one, edited in place.
				{Author: "goobers", Body: renderVerdictComment(tc.sticky)},
				{Author: "goobers", Body: rejection},
				// An untrusted author cannot answer the finding for the instance,
				// and neither can unmarked text carrying a payload, such as a
				// remediation response quoting agent output.
				{Author: "mallory", Body: renderVerdictComment(answered)},
				{Author: "goobers", Body: "Remediation response.\n\n" + forged},
			}
			verdict := gatherPRVerdict("", providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}, 55, comments, "goobers")
			if verdict == nil {
				t.Fatal("gatherPRVerdict returned no verdict")
			}
			if tc.wantPass {
				if verdict.Decision != apiv1.VerdictPass || len(verdict.Findings) != 0 {
					t.Fatalf("verdict = %+v, want the pass unchanged", verdict)
				}
				return
			}
			if verdict.Decision != apiv1.VerdictNeedsChanges || len(verdict.Findings) != 1 || verdict.Findings[0].ID != unboundedLineageID {
				t.Fatalf("verdict = %+v, want needs-changes carrying %s", verdict, unboundedLineageID)
			}
			if !verdictHasSubstantiveFindingForPR(verdict, 55, apiv1.SeverityInfo) {
				t.Fatal("the carried finding is not substantive for remediation routing")
			}
		})
	}
}

// The reported sequence on a current branch: green CI, a retained
// needs-remediation label, and two instance verdict comments. update-behind-pr
// must not clear the label as "branch is current" until the finding is
// explicitly answered.
func TestUpdateBehindPRDoesNotHealCurrentBranchOverUnansweredFinding(t *testing.T) {
	const head, base = "head-sha", "opening-base-sha"
	rejection := apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "Unbounded lineage lookup.", HeadSHA: head, BaseSHA: base, Findings: []apiv1.Finding{unboundedLineageFinding()}}
	generic := apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "Trace text is fixed.", HeadSHA: head, BaseSHA: base}
	answered := generic
	answered.Dispositions = rejectUnboundedLineage()
	for _, tc := range []struct {
		name     string
		comments []map[string]interface{}
		wantFull bool
	}{
		{
			name:     "generic pass after a current-head rejection",
			comments: []map[string]interface{}{verdictCommentFixture(1, "merge-review-bot", generic), verdictCommentFixture(2, "merge-review-bot", rejection)},
			wantFull: true,
		},
		{
			name:     "pass that rejects the finding with a reason",
			comments: []map[string]interface{}{verdictCommentFixture(1, "merge-review-bot", answered), verdictCommentFixture(2, "merge-review-bot", rejection)},
		},
		{
			name:     "generic pass with no earlier finding",
			comments: []map[string]interface{}{verdictCommentFixture(1, "merge-review-bot", generic)},
		},
		{
			name:     "generic pass after the base moved",
			comments: []map[string]interface{}{verdictCommentFixture(1, "merge-review-bot", movedBasePass(generic)), verdictCommentFixture(2, "merge-review-bot", rejection)},
			wantFull: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := captureMergeSafetyBlocks(t)
			mergeable := true
			state := &updateBehindServer{
				mergeable: &mergeable, current: true,
				labels: []string{needsRemediationLabel}, comments: tc.comments,
			}
			stdout, _, result := runUpdateBehindPRTest(t, state)
			if got := result["needsFullRemediation"] == "true"; got != tc.wantFull {
				t.Fatalf("result = %v (stdout %q), want needsFullRemediation=%v", result, stdout, tc.wantFull)
			}
			if result["selectedNumber"] != "55" {
				t.Fatalf("result = %v, want PR #55 selected", result)
			}
			kept := strings.Join(state.labels, ",") == needsRemediationLabel
			if kept != tc.wantFull {
				t.Fatalf("labels = %v, want %s kept=%v", state.labels, needsRemediationLabel, tc.wantFull)
			}
			if strings.Contains(stdout, "branch is current; cleared") == tc.wantFull {
				t.Fatalf("stdout = %q", stdout)
			}
			got := blocks()
			if !tc.wantFull {
				if len(got) != 0 {
					t.Fatalf("merge-safety blocks = %v, want none", got)
				}
				return
			}
			if len(got) != 1 || got[0]["stage"] != "update-behind-pr" || got[0]["action"] != "route-full-remediation" ||
				!reflect.DeepEqual(got[0]["findingIds"], []string{unboundedLineageID}) || got[0]["headSha"] != head {
				t.Fatalf("merge-safety blocks = %v, want one update-behind-pr route naming %s", got, unboundedLineageID)
			}
		})
	}
}

// apply-verdict publishes the authoritative verdict for a head and base: a
// later generic pass is published as needs-changes carrying the earlier
// finding, and only an explicit disposition publishes a pass.
func TestApplyVerdictCarriesUnansweredFindingIntoLaterPass(t *testing.T) {
	const (
		prNumber = 5886
		head     = "a00172a5f4f4d1d9feefb82ab4a7319611525f76"
		base     = "f0852ef64d563b960550c2eb4629932282c9e537"
	)
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(prNumber, "bounded lineage")
	files := []fakePRFile{{path: "internal/readservice/runs.go", status: "modified", additions: 40}}
	server.addOpenPR(prNumber, "goobers/implementation/lineage", "main", head, base, false, nil, files)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "review-1")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_REVIEW", "review-token")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "5886")
	t.Setenv("GOOBERS_REPO_PROVIDER", "")
	t.Setenv("GOOBERS_REPO_OWNER", "")
	t.Setenv("GOOBERS_REPO_NAME", "")
	t.Chdir(t.TempDir())
	blocks := captureMergeSafetyBlocks(t)

	publish := func(runID string, verdict apiv1.Verdict) (map[string]string, apiv1.Verdict) {
		t.Helper()
		t.Setenv("GOOBERS_RUN_ID", runID)
		t.Setenv("GOOBERS_INPUT_REVIEWDIGEST", "review-"+runID)
		verdict.HeadSHA, verdict.BaseSHA = head, base
		seedGateVerdictJournal(t, root, runID, verdict)
		code, stdout, stderr := runArgs(t, "apply-verdict", root)
		if code != 0 {
			t.Fatalf("%s apply-verdict: code=%d stdout=%q stderr=%q", runID, code, stdout, stderr)
		}
		var result map[string]string
		data, err := os.ReadFile("verdict-result.json")
		if err != nil {
			t.Fatalf("%s read result: %v", runID, err)
		}
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatalf("%s unmarshal result: %v", runID, err)
		}
		comments, _ := fakeIssueComments(t, server, prNumber)
		if len(comments) != 1 {
			t.Fatalf("%s comments = %q, want the one sticky status comment", runID, comments)
		}
		posted, ok := parseVerdictComment(comments[0])
		if !ok {
			t.Fatalf("%s status has no verdict payload: %q", runID, comments[0])
		}
		return result, posted
	}

	result, _ := publish("review-rejects", apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges, Summary: "Unbounded lineage lookup.", Rationale: "The lookup is unbounded.",
		Findings: []apiv1.Finding{unboundedLineageFinding()},
	})
	if result["decision"] != string(apiv1.VerdictNeedsChanges) || !issueHasLabel(server, prNumber, needsRemediationLabel) {
		t.Fatalf("first review: result=%v, want needs-changes with %s", result, needsRemediationLabel)
	}
	if got := blocks(); len(got) != 0 {
		t.Fatalf("a first rejection is not a merge-safety block: %v", got)
	}

	// The later generic pass on the same head and base.
	result, posted := publish("review-generic-pass", apiv1.Verdict{
		Decision: apiv1.VerdictPass, Summary: "Trace text is fixed.", Rationale: "The trace text issue is addressed.",
	})
	if result["decision"] != string(apiv1.VerdictNeedsChanges) {
		t.Fatalf("generic pass result = %v, want it published as needs-changes so merge-pr never runs", result)
	}
	if posted.Decision != apiv1.VerdictNeedsChanges || len(posted.Findings) != 1 || posted.Findings[0].ID != unboundedLineageID {
		t.Fatalf("published verdict = %+v, want needs-changes carrying %s", posted, unboundedLineageID)
	}
	if !strings.Contains(posted.Rationale, unboundedLineageID) {
		t.Fatalf("published rationale %q does not say what was carried", posted.Rationale)
	}
	if !issueHasLabel(server, prNumber, needsRemediationLabel) {
		t.Fatalf("generic pass dropped %s", needsRemediationLabel)
	}
	state, ok, err := loadVerdictState(root, providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}, prNumber)
	if err != nil || !ok || state.Decision != apiv1.VerdictNeedsChanges {
		t.Fatalf("authoritative verdict state = %+v ok=%v err=%v, want the carried needs-changes", state, ok, err)
	}
	got := blocks()
	if len(got) != 1 || got[0]["stage"] != "apply-verdict" || got[0]["action"] != "downgrade-pass" {
		t.Fatalf("merge-safety blocks = %v, want one apply-verdict downgrade", got)
	}

	// Neither a bare fail nor a typed rejection can erase the finding from
	// the single-slot records, and both still publish.
	repoRef := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	for _, fail := range []apiv1.Verdict{
		{Decision: apiv1.VerdictFail, Summary: "Rejected.", Rationale: "No reason code."},
		{Decision: apiv1.VerdictFail, ReasonCode: apiv1.VerdictReasonImplementationRejected, Summary: "Rejected.", Rationale: "The approach is wrong."},
	} {
		result, posted = publish("review-fail-"+string(fail.ReasonCode), fail)
		if result["decision"] != string(apiv1.VerdictFail) || posted.Decision != apiv1.VerdictFail || len(posted.Findings) != 0 {
			t.Fatalf("fail %q: result=%v verdict=%+v, want the fail published as reviewed", fail.ReasonCode, result, posted)
		}
		if len(posted.StandingFindings) != 1 || posted.StandingFindings[0].ID != unboundedLineageID {
			t.Fatalf("fail %q: StandingFindings = %+v, want the ledger carried", fail.ReasonCode, posted.StandingFindings)
		}
		comments, _ := fakeIssueComments(t, server, prNumber)
		records, err := verdictRecordsForPR(root, repoRef, prNumber, []providers.Comment{{Author: server.authenticatedLogin, Body: comments[0]}}, server.authenticatedLogin)
		if err != nil {
			t.Fatal(err)
		}
		if got := findingledger.Outstanding(records, head, prFindingPolicy(prNumber)); len(got) != 1 {
			t.Fatalf("fail %q: outstanding = %v, want the finding still blocking merge-pr", fail.ReasonCode, got)
		}
	}

	// The escape hatch: a re-review on the same head (an operator rerun with
	// instructions, or --no-verdict-cache) that rejects the finding with a
	// reason publishes the pass and clears merge-pr's conjunct.
	result, posted = publish("review-answers", apiv1.Verdict{
		Decision: apiv1.VerdictPass, Summary: "Ready to merge.", Rationale: "The lineage finding does not hold.",
		Dispositions: rejectUnboundedLineage(),
	})
	if result["decision"] != string(apiv1.VerdictPass) || posted.Decision != apiv1.VerdictPass || len(posted.Findings) != 0 {
		t.Fatalf("answered pass: result=%v verdict=%+v, want a clean pass", result, posted)
	}
	if !reflect.DeepEqual(posted.Dispositions, rejectUnboundedLineage()) {
		t.Fatalf("published dispositions = %+v, want the rejection recorded", posted.Dispositions)
	}
	comments, _ := fakeIssueComments(t, server, prNumber)
	if !strings.Contains(comments[0], "- rejected `"+unboundedLineageID+"`: The scan is bounded") {
		t.Fatalf("status comment does not show the rejection to a human: %q", comments[0])
	}
	if got := blocks(); len(got) != 3 {
		t.Fatalf("merge-safety blocks = %v, want the downgrade and the two carried fails only", got)
	}
	records, err := verdictRecordsForPR(root, repoRef, prNumber, []providers.Comment{{Author: server.authenticatedLogin, Body: comments[0]}}, server.authenticatedLogin)
	if err != nil {
		t.Fatal(err)
	}
	if got := findingledger.Outstanding(records, head, prFindingPolicy(prNumber)); len(got) != 0 {
		t.Fatalf("outstanding after the answered pass = %v, want none", got)
	}
}

// merge-pr refuses while an earlier finding on the live head and base stands,
// even with a trusted sticky pass pinned to that head and base.
func TestMergePRRefusesOverUnansweredSameHeadFinding(t *testing.T) {
	const head, base = "head123", "base456"
	rejection := renderVerdictComment(apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges, Summary: "Unbounded lineage lookup.", HeadSHA: head, BaseSHA: base,
		Findings: []apiv1.Finding{unboundedLineageFinding()},
	})
	for _, tc := range []struct {
		name        string
		prior       []map[string]interface{}
		wantMerged  bool
		wantBlocked bool
	}{
		{
			name:        "earlier rejection never answered",
			prior:       []map[string]interface{}{{"id": 7, "body": rejection, "user": map[string]string{"login": "goobers"}}},
			wantBlocked: true,
		},
		{
			// main advanced between the reviews: the PR's own diff did not change.
			name: "earlier rejection on an older base never answered",
			prior: []map[string]interface{}{{"id": 7, "user": map[string]string{"login": "goobers"}, "body": renderVerdictComment(apiv1.Verdict{
				Decision: apiv1.VerdictNeedsChanges, Summary: "Unbounded lineage lookup.", HeadSHA: head, BaseSHA: "older-base",
				Findings: []apiv1.Finding{unboundedLineageFinding()},
			})}},
			wantBlocked: true,
		},
		{
			name: "earlier rejection answered by a later record",
			prior: []map[string]interface{}{
				{"id": 7, "body": rejection, "user": map[string]string{"login": "goobers"}},
				{"id": 8, "body": renderVerdictComment(apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "Answered.", HeadSHA: head, BaseSHA: base, Dispositions: rejectUnboundedLineage()}), "user": map[string]string{"login": "goobers"}},
			},
			wantMerged: true,
		},
		{
			name:       "rejection by an untrusted author",
			prior:      []map[string]interface{}{{"id": 7, "body": rejection, "user": map[string]string{"login": "mallory"}}},
			wantMerged: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := captureMergeSafetyBlocks(t)
			st := &mergePRServerState{checkState: "success", headSHA: head, baseSHA: base, extraComments: tc.prior}
			server := newMergePRServer(t, "your-org", "your-repo", st)
			root, dir := mergePREnv(t, server.URL, false, map[string]string{
				"pullNumber": "9", "verdict": "pass", "headSha": head, "baseSha": base,
			})
			code, stdout, stderr := runArgs(t, "merge-pr", root)
			if code != 0 {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
			result := readMergeResult(t, dir)
			merged, _ := result["merged"].(bool)
			if merged != tc.wantMerged || (st.mergeCalls == 1) != tc.wantMerged {
				t.Fatalf("result = %+v, merge calls = %d, want merged=%v", result, st.mergeCalls, tc.wantMerged)
			}
			got := blocks()
			if !tc.wantBlocked {
				if len(got) != 0 {
					t.Fatalf("merge-safety blocks = %v, want none", got)
				}
				return
			}
			reason, _ := result["reason"].(string)
			if !strings.Contains(reason, unboundedLineageID) || strings.Contains(reason, "; ") {
				t.Fatalf("reason = %q, want the finding named in one reason", reason)
			}
			if len(got) != 1 || got[0]["stage"] != "merge-pr" || got[0]["action"] != "refuse-merge" || got[0]["baseSha"] != base {
				t.Fatalf("merge-safety blocks = %v, want one merge-pr refusal", got)
			}
		})
	}
}

// gather-sibling-context lists the unanswered finding for the reviewer, so a
// later review can answer it instead of passing over it in silence.
func TestGatherSiblingContextListsStandingFindings(t *testing.T) {
	const (
		selectedNumber = 10
		head, base     = "sha10head", "shamainbase"
	)
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(selectedNumber, "Selected PR")
	server.addOpenPR(selectedNumber, "goobers/implementation/run-10", "main", head, base,
		false, nil, []fakePRFile{{path: "internal/readservice/runs.go", status: "modified", additions: 5}})
	server.addComment(selectedNumber, renderVerdictComment(apiv1.Verdict{
		Decision: apiv1.VerdictPass, Summary: "Trace text is fixed.", HeadSHA: head, BaseSHA: base,
	}))
	server.addComment(selectedNumber, renderVerdictComment(apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges, Summary: "Unbounded lineage lookup.", HeadSHA: head, BaseSHA: base,
		Findings: []apiv1.Finding{unboundedLineageFinding()},
	}))
	// A finding pinned to an older head is not standing on this one.
	server.addComment(selectedNumber, renderVerdictComment(apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges, Summary: "Old head.", HeadSHA: "older-head", BaseSHA: base,
		Findings: []apiv1.Finding{{ID: "old-head-finding", Severity: apiv1.SeverityError, Class: apiv1.FindingSubstantive, Message: "Fixed since."}},
	}))
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "10")
	t.Chdir(t.TempDir())
	if code, stdout, stderr := runArgs(t, "gather-sibling-context", root); code != 0 {
		t.Fatalf("gather-sibling-context: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile("sibling-context.json")
	if err != nil {
		t.Fatalf("read sibling-context.json: %v", err)
	}
	var out struct {
		StandingFindings []apiv1.Finding `json:"standingFindings"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal sibling-context.json: %v", err)
	}
	if len(out.StandingFindings) != 1 || out.StandingFindings[0].ID != unboundedLineageID {
		t.Fatalf("standingFindings = %+v, want only %s", out.StandingFindings, unboundedLineageID)
	}
}

func movedBasePass(v apiv1.Verdict) apiv1.Verdict {
	v.BaseSHA = "newer-base-sha"
	return v
}

// corruptVerdictState makes the PR's scheduler-state verdict unreadable.
func corruptVerdictState(t *testing.T, root string, repo providers.RepositoryRef, number int) {
	t.Helper()
	store, err := openStageStateStore(instance.NewLayout(root))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), verdictStateKey(verdictRecordKey(repo, number)), []byte("{not json"), ""); err != nil {
		t.Fatal(err)
	}
}

// A ledger that cannot be read never lets the API-only lane clear the label.
func TestUpdateBehindPRFailsClosedOnAnUnreadableLedger(t *testing.T) {
	mergeable := true
	generic := apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "Fine.", HeadSHA: "head-sha", BaseSHA: "opening-base-sha"}
	state := &updateBehindServer{
		mergeable: &mergeable, current: true,
		labels: []string{needsRemediationLabel}, comments: []map[string]interface{}{verdictCommentFixture(1, "merge-review-bot", generic)},
	}
	root, workspace := setupUpdateBehindPRTest(t, state)
	corruptVerdictState(t, root, providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}, 55)
	code, stdout, stderr, _ := invokeUpdateBehindPRTest(t, root, workspace)
	if code == 0 {
		t.Fatalf("code = 0, stdout = %q, want the stage to fail on an unreadable ledger", stdout)
	}
	if strings.Join(state.labels, ",") != needsRemediationLabel {
		t.Fatalf("labels = %v, want %s kept (stderr %q)", state.labels, needsRemediationLabel, stderr)
	}
}

// The canonical status comment is edited in place, so on GitHub/Gitea it is
// the newest record however old its position; Azure DevOps threads are
// append-only. Healers and merge-pr read the same order.
func TestVerdictRecordsForPRReadsPublicationOrder(t *testing.T) {
	const head = "head-sha"
	rejection := renderVerdictComment(apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "Unbounded.", HeadSHA: head, BaseSHA: "b", Findings: []apiv1.Finding{unboundedLineageFinding()}})
	answer := renderVerdictComment(apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "Answered.", HeadSHA: head, BaseSHA: "b", Dispositions: rejectUnboundedLineage()})
	comments := []providers.Comment{{Author: "goobers", Body: answer}, {Author: "goobers", Body: rejection}}
	for _, tc := range []struct {
		provider providers.ProviderKind
		standing int
	}{
		{providers.ProviderGitHub, 0},
		{providers.ProviderADO, 1},
	} {
		repo := providers.RepositoryRef{Provider: tc.provider, Owner: "your-org", Name: "your-repo"}
		records, err := verdictRecordsForPR("", repo, 55, comments, "goobers")
		if err != nil {
			t.Fatal(err)
		}
		if got := findingledger.Outstanding(records, head, prFindingPolicy(55)); len(got) != tc.standing {
			t.Fatalf("%s: outstanding = %v, want %d", tc.provider, got, tc.standing)
		}
	}
}

type loginStub struct{ err error }

func (s loginStub) AuthenticatedLogin(context.Context) (string, error) { return "", s.err }

// merge-pr refuses when it cannot name the identity whose verdicts it trusts,
// or cannot read the scheduler-state record.
func TestMergePRFindingCheckFailsClosed(t *testing.T) {
	poll := providers.PullRequestPollResult{HeadSHA: "head-sha", BaseSHA: "base-sha"}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	for name, provider := range map[string]any{
		"login lookup fails":  loginStub{err: errors.New("401")},
		"no identity to name": struct{}{},
	} {
		if got := outstandingFindingMergeRefusals(context.Background(), "", provider, repo, "9", "", poll); len(got) != 1 || !strings.Contains(got[0], "could not read") {
			t.Fatalf("%s: refusals = %v, want one unreadable-evidence refusal", name, got)
		}
	}
	root := initDemo(t)
	corruptVerdictState(t, root, repo, 9)
	if got := outstandingFindingMergeRefusals(context.Background(), root, struct{}{}, repo, "9", "goobers", poll); len(got) != 1 || !strings.Contains(got[0], "could not read") {
		t.Fatalf("corrupt state: refusals = %v, want one unreadable-evidence refusal", got)
	}
	if got := outstandingFindingMergeRefusals(context.Background(), "", struct{}{}, repo, "9", "goobers", poll); len(got) != 0 {
		t.Fatalf("no records: refusals = %v, want none", got)
	}
}

// disguisedStandingSticky is the canonical status comment as apply-verdict
// publishes it when a review reuses a standing finding's id on a
// non-blocking finding: the earlier needs-changes record has been replaced.
func disguisedStandingSticky(t *testing.T, head, base string, finding apiv1.Finding) apiv1.Verdict {
	t.Helper()
	prior := []apiv1.Verdict{{Decision: apiv1.VerdictNeedsChanges, Summary: "Unbounded.", HeadSHA: head, BaseSHA: base, Findings: []apiv1.Finding{finding}}}
	disguise := apiv1.Finding{ID: finding.ID, Severity: apiv1.SeverityWarning, Class: apiv1.FindingRebaseNeeded, Message: "Rebase onto the base branch."}
	next := apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "Behind base.", HeadSHA: head, BaseSHA: base, Findings: []apiv1.Finding{disguise}}
	folded, _ := carryOutstandingFindings(prior, next, 55, false)
	if len(folded.StandingFindings) != 1 {
		t.Fatalf("published record = %+v, want the finding standing", folded)
	}
	return folded
}

// The healers' half of the reused-id case: a standing finding whose id a
// review reused on a rebase-needed finding must still route a behind PR to
// full remediation. Updating the branch would move the head and start an
// empty ledger, after which a silent pass merges. The same holds for a
// standing finding below the healers' severity floor, since merge-pr blocks
// on it.
func TestUpdateBehindPRRoutesAStandingFindingWhateverItsDisguiseOrSeverity(t *testing.T) {
	warning := unboundedLineageFinding()
	warning.Severity = apiv1.SeverityWarning
	for _, tc := range []struct {
		name        string
		finding     apiv1.Finding
		minSeverity string
	}{
		{name: "id reused on a rebase-needed finding", finding: unboundedLineageFinding()},
		{name: "standing finding below the healers' floor", finding: warning, minSeverity: "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.minSeverity != "" {
				t.Setenv(executor.InputEnvVar("minSeverity"), tc.minSeverity)
			}
			sticky := disguisedStandingSticky(t, "head-sha", "opening-base-sha", tc.finding)
			mergeable := true
			state := &updateBehindServer{
				mergeable: &mergeable,
				labels:    []string{needsRemediationLabel},
				comments:  []map[string]interface{}{verdictCommentFixture(1, "merge-review-bot", sticky)},
			}
			_, _, result := runUpdateBehindPRTest(t, state)
			if result["needsFullRemediation"] != "true" || state.updateCalls != 0 {
				t.Fatalf("result = %v, update-branch calls = %d, want full remediation and no branch update", result, state.updateCalls)
			}
			if strings.Join(state.labels, ",") != needsRemediationLabel {
				t.Fatalf("labels = %v, want %s kept", state.labels, needsRemediationLabel)
			}
		})
	}
}

// gather-pr-context and the sibling-overlap narrowing read the same record:
// the substantive finding replaces its disguise in Findings, and no severity
// floor or overlap attribution narrows it away.
func TestRemediationReadersSeeADisguisedStandingFinding(t *testing.T) {
	sticky := disguisedStandingSticky(t, "head-sha", "base-sha", unboundedLineageFinding())
	comments := []providers.Comment{{Author: "goobers", Body: renderVerdictComment(sticky)}}
	verdict := gatherPRVerdict("", providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}, 55, comments, "goobers")
	if verdict == nil || len(verdict.Findings) != 1 || verdict.Findings[0].Class != apiv1.FindingSubstantive {
		t.Fatalf("verdict = %+v, want the substantive finding listed for the brief", verdict)
	}
	if !verdictHasSubstantiveFindingForPR(verdict, 55, apiv1.SeverityCritical) {
		t.Fatal("hasSubstantiveFindings is false under a critical floor")
	}
	if !verdictHasIndependentSubstantiveFindingForPR(verdict, 55, []int{56}, apiv1.SeverityCritical) {
		t.Fatal("sibling-overlap narrowing dropped a standing finding")
	}
}
