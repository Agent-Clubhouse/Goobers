package main

import (
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// A person who mentions a Goobers payload — in inline code, a quote, or a
// fenced excerpt — is still a person: only a line that starts with a payload,
// as every renderer writes it, marks a bookkeeping comment.
func TestCommentClassifierPayloadMentionStaysHuman(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for name, body := range map[string]string{
		"inline code": "Please don't render `<!-- verdict-json: ` twice here; fix before merge.",
		"quoted":      "> <!-- remediation-state: {} -->\nThis state looks wrong.",
		"fenced":      "The sticky shows:\n```\n<!-- remediation-state: {\"cycles\":3} -->\n```\nWhy three cycles?",
	} {
		t.Run(name, func(t *testing.T) {
			comment := providers.Comment{Author: "alice", Body: body, CreatedAt: &at}
			for _, mode := range []string{commentIdentityShared, commentIdentityDedicated} {
				classifier := newCommentClassifier(loginCommentIdentity("goobers-bot"), commentIdentitySettings{mode: mode})
				if got := classifier.origin(comment); got != commentFromHuman {
					t.Fatalf("%s: origin = %v, want human", mode, got)
				}
				if _, fresh, _ := latestUnaddressedHumanComment([]providers.Comment{comment}, classifier); !fresh {
					t.Fatalf("%s: a person's comment mentioning a payload was not fresh feedback", mode)
				}
			}
		})
	}
	state, err := remediationStateComment(remediationState{Cycles: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !goobersBookkeepingComment(state) {
		t.Fatal("a rendered remediation-state comment is no longer recognised as bookkeeping")
	}
}

// An unterminated fence in agent-written prose (a verdict rationale with a
// truncated code block) must not hide the payload the renderer appends after
// it: otherwise the verdict passes as a Goobers answer and acknowledges every
// earlier human comment.
func TestBookkeepingPayloadSurvivesUnclosedFence(t *testing.T) {
	verdict := renderVerdictComment(apiv1.Verdict{Decision: apiv1.VerdictPass, Rationale: "Checked:\n```go\nfunc x() {}\n"})
	if !goobersBookkeepingComment(verdict) {
		t.Fatal("a verdict whose rationale leaves a code fence open is no longer bookkeeping")
	}
	if !goobersBookkeepingComment(stampOwnFixtureBody(verdict, "comment")) {
		t.Fatal("a marked verdict with an open fence in its rationale is no longer bookkeeping")
	}
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	later := at.Add(time.Hour)
	human := providers.Comment{Author: "alice", Body: "please fix X", CreatedAt: &at}
	marked := providers.Comment{Author: "goobers-bot", Body: stampOwnFixtureBody(verdict, "comment"), CreatedAt: &later}
	classifier := newCommentClassifier(loginCommentIdentity("goobers-bot"), commentIdentitySettings{mode: commentIdentityDedicated})
	if _, fresh, _ := latestUnaddressedHumanComment([]providers.Comment{human, marked}, classifier); !fresh {
		t.Fatal("a verdict with an open fence acknowledged a person's comment")
	}
}
