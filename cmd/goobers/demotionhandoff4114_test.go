package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

// record-merge-refusal applies {merge-demoted, needs-remediation} in ONE
// mutation so the demoted lander has a path to move its head, and
// mergeDemotionState says the same in prose. So merge-demoted without
// needs-remediation, needs-human or merge-escalated is a state the design says
// cannot exist — yet #3891 and #3900 sat in it, CONFLICTING with 22/22 green
// checks, refused by both lanes, because escalate() removed the label and the
// #4109 self-heal could not reach a PR that had already left the escalated
// bucket.
func TestReconcileRestoresTheDemotionHandoff(t *testing.T) {
	demotedHead := "6b443d5973aa0a2f4e1b0d1c2a3948576e1f0a2b"
	snapshot := demotionStateComment(demotedHead)

	cases := []struct {
		name   string
		pr     providers.PullRequestSummary
		want   bool
		reason string
	}{
		{
			name: "the stranded live shape is handed back",
			pr: providers.PullRequestSummary{
				Number: 3891, HeadSHA: demotedHead,
				Head:   providerBranchNamespace() + "implementation/15343165797e1c6868608c46f3ed4c54",
				Labels: []string{mergeDemotedLabel},
			},
			want:   true,
			reason: "demoted, unparked, and in no lane at all",
		},
		{
			name: "a PR that already has the label is left alone",
			pr: providers.PullRequestSummary{
				Number: 3894, HeadSHA: demotedHead,
				Labels: []string{mergeDemotedLabel, needsRemediationLabel},
			},
			want:   false,
			reason: "the invariant already holds; a second write would be noise",
		},
		{
			name: "a needs-human park is not overridden",
			pr: providers.PullRequestSummary{
				Number: 3908, HeadSHA: demotedHead,
				Labels: []string{mergeDemotedLabel, providers.LabelNeedsHuman},
			},
			want:   false,
			reason: "the circuit breaker parked it deliberately",
		},
		{
			name: "an escalated park is not overridden",
			pr: providers.PullRequestSummary{
				Number: 3968, HeadSHA: demotedHead,
				Labels: []string{mergeDemotedLabel, remediationEscalatedLabel},
			},
			want:   false,
			reason: "merge-escalated is a human-only bucket",
		},
		{
			name: "a PR that is not demoted is untouched",
			pr: providers.PullRequestSummary{
				Number: 4000, HeadSHA: demotedHead,
				Labels: []string{},
			},
			want:   false,
			reason: "this sweep only asserts the demotion handoff",
		},
		{
			name: "a demotion whose head advanced belongs to the un-demote sweep",
			pr: providers.PullRequestSummary{
				Number: 4001, HeadSHA: "ffffffffffffffffffffffffffffffffffffffff",
				Labels: []string{mergeDemotedLabel},
			},
			want:   false,
			reason: "demotionStillHolds is false; the demotion lifts entirely instead",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := &demotionHandoffStub{comments: snapshot}
			handed, errs := restoreDemotionHandoffFrom(context.Background(), provider, providers.RepositoryRef{
				Provider: providers.ProviderGitHub, Owner: "Agent-Clubhouse", Name: "Goobers",
			}, []providers.PullRequestSummary{tc.pr}, io.Discard)
			if len(errs) != 0 {
				t.Fatalf("errs = %v", errs)
			}
			if got := len(handed) == 1; got != tc.want {
				t.Fatalf("handed = %v, want %v — %s", handed, tc.want, tc.reason)
			}
			if !tc.want {
				if len(provider.updates) != 0 {
					t.Fatalf("wrote %v; %s", provider.updates, tc.reason)
				}
				return
			}
			if len(provider.updates) != 1 {
				t.Fatalf("updates = %d, want 1", len(provider.updates))
			}
			update := provider.updates[0]
			if len(update.RemoveLabels) != 0 {
				t.Fatalf("RemoveLabels = %v, want none — the demotion itself stands", update.RemoveLabels)
			}
			if !equalStrings(update.AddLabels, []string{needsRemediationLabel}) {
				t.Fatalf("AddLabels = %v, want [%s]", update.AddLabels, needsRemediationLabel)
			}
		})
	}
}

// #4110 removed the "goobers/" literal from postmerge.go's struct field and
// missed the positional argument in postmergereconcile.go — which is the sweep
// that actually runs on every merge-review tick. Scan for a literal namespace
// in any form.
func TestNoProductionPathHardCodesTheBranchNamespace(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	literal := regexp.MustCompile(`(HeadPrefix:\s*"|filterPullRequestsByHeadPrefix\([^,]+,\s*")`)
	var offenders []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if literal.MatchString(line) {
				offenders = append(offenders, filepath.Join(".", name)+":"+lineNo(i+1)+": "+strings.TrimSpace(line))
			}
		}
	}
	if len(offenders) != 0 {
		t.Fatalf("the branch namespace must come from providerBranchNamespace(); a literal is right in tests and wrong in production:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

func lineNo(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func demotionStateComment(head string) []providers.Comment {
	return []providers.Comment{{Body: "**merge refusal recorded**\n\n<!-- merge-demotion-state: " +
		`{"attempts":3,"demoted":true,"headSha":"` + head + `","reason":"merge-conflict","recordedAt":"2026-08-30T21:06:04Z"}` +
		" -->"}}
}

type demotionHandoffStub struct {
	remediationProvider
	comments []providers.Comment
	updates  []providers.UpdateWorkItemRequest
}

func (p *demotionHandoffStub) ListComments(context.Context, providers.RepositoryRef, string) ([]providers.Comment, error) {
	return p.comments, nil
}

func (p *demotionHandoffStub) UpdateWorkItem(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	p.updates = append(p.updates, req)
	return providers.WorkItem{}, nil
}
