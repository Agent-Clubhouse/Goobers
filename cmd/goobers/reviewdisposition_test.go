package main

import (
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestOrderingDeferralPreservesReviewEvidence(t *testing.T) {
	v := apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Rationale: " Wait for #10.\n\nComplete original rationale.\n", Findings: []apiv1.Finding{blockedFinding(10)}}
	got := orderingDeferralVerdict(v)
	if got.Decision != apiv1.VerdictDefer || got.ReasonCode != apiv1.VerdictReasonOrdering || got.Elected || got.Rationale != v.Rationale || !reflect.DeepEqual(got.Findings, v.Findings) {
		t.Fatalf("ordering normalization lost the disposition or evidence: %+v", got)
	}
	v.Findings = append(v.Findings, apiv1.Finding{Severity: apiv1.SeverityError, Class: apiv1.FindingSubstantive, Message: "actual defect"})
	if got := orderingDeferralVerdict(v); !reflect.DeepEqual(got, v) {
		t.Fatalf("ordering normalization hid a repairable defect: %+v", got)
	}
}

func TestLegacyFailAmbiguityIsVisibleWithoutRewritingVerdict(t *testing.T) {
	v := apiv1.Verdict{Decision: apiv1.VerdictFail, Rationale: "Original reviewer statement."}
	comment := renderVerdictComment(v)
	if !strings.Contains(comment, legacyFailAmbiguous) || publishedVerdictReason(v) != legacyFailAmbiguous {
		t.Fatalf("legacy fail silently treated as typed rejection: %s", comment)
	}
	parsed, ok := parseVerdictComment(comment)
	if !ok || !reflect.DeepEqual(parsed, v) {
		t.Fatalf("legacy evidence rewritten: %+v", parsed)
	}
	v.ReasonCode = apiv1.VerdictReasonImplementationRejected
	if strings.Contains(renderVerdictComment(v), legacyFailAmbiguous) || publishedVerdictReason(v) != string(v.ReasonCode) {
		t.Fatal("explicit rejection reported as ambiguous")
	}
}

func TestTerminalVerdictRequirementRejectsFixableAndOrderingFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    apiv1.Verdict
		want string
	}{
		{name: "ordering fail is rejected", v: apiv1.Verdict{Decision: apiv1.VerdictFail, ReasonCode: apiv1.VerdictReasonOrdering, Rationale: "wait for sibling"}, want: "terminal fail verdict"},
		{name: "cross-pr-blocked fail is rejected", v: apiv1.Verdict{Decision: apiv1.VerdictFail, ReasonCode: apiv1.VerdictReasonImplementationRejected, Rationale: "This PR is blocked by #10.", Findings: []apiv1.Finding{{Severity: apiv1.SeverityInfo, Class: apiv1.FindingCrossPRBlocked, Message: "wait on #10", BlockingPRs: []int{10}}}}, want: "terminal fail verdict"},
		{name: "unsalvageable requires rationale", v: apiv1.Verdict{Decision: apiv1.VerdictFail, ReasonCode: apiv1.VerdictReasonUnsalvageableDesign}, want: "unsalvageable-design fail verdict requires rationale"},
		{name: "unsalvageable rationale must explain why code changes cannot repair", v: apiv1.Verdict{Decision: apiv1.VerdictFail, ReasonCode: apiv1.VerdictReasonUnsalvageableDesign, Rationale: "Wait for sibling #10."}, want: "unsalvageable-design fail verdict requires rationale"},
		{name: "typed failure remains valid", v: apiv1.Verdict{Decision: apiv1.VerdictFail, ReasonCode: apiv1.VerdictReasonImplementationRejected, Rationale: "Approach violates the required contract."}, want: ""},
		{name: "valid unsalvageable rationale passes", v: apiv1.Verdict{Decision: apiv1.VerdictFail, ReasonCode: apiv1.VerdictReasonUnsalvageableDesign, Rationale: "The underlying design cannot be repaired by ordinary code changes; the approach itself must be replaced."}, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := terminalVerdictRequirement(tc.v)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("terminalVerdictRequirement() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("terminalVerdictRequirement() = %v, want error containing %q", err, tc.want)
			}
		})
	}
}

func TestValidateVerdictForPublishRejectsContradictoryTerminalStatus(t *testing.T) {
	v := apiv1.Verdict{Decision: apiv1.VerdictFail, ReasonCode: apiv1.VerdictReasonOrdering, Rationale: "Wait for sibling #10."}
	if err := validateVerdictForPublish(v); err == nil {
		t.Fatal("validateVerdictForPublish accepted a terminal fail with ordering semantics")
	}
}

func TestApplyVerdictRejectsContradictoryTerminalFailBeforeEscalating(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	const prNumber = 533
	server.addIssue(prNumber, "Contradictory fail")
	server.addOpenPR(prNumber, "goobers/implementation/contradictory", "main", "selected-head", "main-base", false, nil, []fakePRFile{{path: "cmd/goobers/reviewdisposition.go", status: "modified", additions: 2}})

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "contradictory-fail")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_REVIEW", "review-token")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "533")
	seedGateVerdictJournal(t, root, "contradictory-fail", apiv1.Verdict{
		Decision:   apiv1.VerdictFail,
		ReasonCode: apiv1.VerdictReasonUnsalvageableDesign,
		Rationale:  "Wait for sibling #10 before any code changes.",
		HeadSHA:    "selected-head",
		BaseSHA:    "main-base",
		Findings: []apiv1.Finding{{
			Severity:    apiv1.SeverityInfo,
			Class:       apiv1.FindingCrossPRBlocked,
			Message:     "wait on #10",
			BlockingPRs: []int{10},
		}},
	})

	t.Chdir(t.TempDir())
	code, stdout, stderr := runArgs(t, "apply-verdict", root)
	if code == 0 {
		t.Fatalf("apply-verdict: code = 0, want non-zero on contradictory terminal fail; stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(stderr, "reviewer verdict contract") && !strings.Contains(stderr, "unsalvageable-design fail verdict requires rationale") {
		t.Fatalf("stderr = %q, want reviewer-contract rejection for contradictory fail", stderr)
	}
	if issueHasLabel(server, prNumber, remediationEscalatedLabel) {
		t.Fatalf("labels = %v, want contradictory fail to fail closed without escalation", server.issues[prNumber].labels)
	}
}
