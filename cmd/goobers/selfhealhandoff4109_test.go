package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

// escalate() removes needsRemediationLabel when it parks a PR. Until #4109 the
// self-heal that lifts the park only removed merge-escalated, so the PR landed
// in NEITHER lane: pr-remediation's remediationPriorityFor returns none without
// the label, and merge-review's pr-select skips a still-demoted PR whose head
// never advances — because nothing is left to remediate it.
//
// MEASURED live: #3891 and #3900 were escalated 2026-08-30 (needs-remediation
// removed at 20:43:27 and 21:55:57), released by #4089 on 2026-09-01T01:48, and
// then refused by both lanes:
//
//	pr-remediation  no work: targeted PR #3891 does not need remediation this
//	                cycle (no goobers:needs-remediation label, CI is not
//	                failing, and it is not a crowned behind-base lander)
//	merge-review    no work: no eligible PR to select this cycle
func TestSelfHealedEscalationHandsThePRBackToRemediation(t *testing.T) {
	// #3891's live payload verbatim, with the head advanced past the recorded
	// escalation snapshot — the exit the label's own comment advertises.
	released := providers.PullRequestSummary{
		Number:  3891,
		Head:    "goobernetes/implementation/15343165797e1c6868608c46f3ed4c54",
		HeadSHA: "6b443d5973aa0a2f4e1b0d1c2a3948576e1f0a2b",
		Labels:  []string{remediationEscalatedLabel, mergeDemotedLabel},
	}
	provider := &selfHealStubProvider{}

	unparked, errs := unparkSelfHealedEscalationsFrom(context.Background(), provider, providers.RepositoryRef{
		Provider: providers.ProviderGitHub, Owner: "Agent-Clubhouse", Name: "Goobers",
	}, 3904, []providers.PullRequestSummary{released}, io.Discard)
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	if len(unparked) != 1 || unparked[0] != 3891 {
		t.Fatalf("unparked = %v, want [3891]", unparked)
	}
	if len(provider.updates) != 1 {
		t.Fatalf("updates = %d, want exactly 1 — the two halves must not race as separate mutations", len(provider.updates))
	}
	update := provider.updates[0]
	if got := sortedCopy(update.RemoveLabels); !equalStrings(got, []string{remediationEscalatedLabel}) {
		t.Fatalf("RemoveLabels = %v, want [%s]", got, remediationEscalatedLabel)
	}
	if got := sortedCopy(update.AddLabels); !equalStrings(got, []string{needsRemediationLabel}) {
		t.Fatalf("AddLabels = %v, want [%s] — without it the PR leaves every lane", got, needsRemediationLabel)
	}

	// The restored label is exactly what the remediation lane selects on.
	released.Labels = []string{mergeDemotedLabel, needsRemediationLabel}
	if got := remediationPriorityFor(released); got != remediationPriorityNeedsRemediation {
		t.Fatalf("remediationPriorityFor = %v, want needs-remediation", got)
	}
}

// unparkSelfHealedDemotions carried the only hard-coded HeadPrefix left in the
// tree, "goobers/", while its sibling one sweep above used
// providerBranchNamespace(). Production's namespace is "goobernetes/", so the
// demotion self-heal listed an empty set for its entire life — every live
// reconcile-post-merge reported "un-demoted 0 self-healed pr(s)".
func TestNoStageHardCodesTheBranchNamespace(t *testing.T) {
	root := "."
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	literal := regexp.MustCompile(`HeadPrefix:\s*"`)
	var offenders []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if literal.MatchString(line) {
				offenders = append(offenders, fmt.Sprintf("%s:%d: %s", filepath.Join(root, name), i+1, strings.TrimSpace(line)))
			}
		}
	}
	if len(offenders) != 0 {
		t.Fatalf("HeadPrefix must come from providerBranchNamespace(); a literal is right in tests and wrong in production:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type selfHealStubProvider struct {
	remediationProvider
	updates []providers.UpdateWorkItemRequest
}

func (p *selfHealStubProvider) ListComments(context.Context, providers.RepositoryRef, string) ([]providers.Comment, error) {
	return []providers.Comment{{Body: "**pr-remediation escalated**\n\n<!-- remediation-state: " +
		`{"cycles":2,"attemptsByCause":{"conflict":1},"headSha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",` +
		`"baseSha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","escalated":true,"remediationAttempted":true,` +
		`"attemptedCauses":["conflict"],"escalatedHeadSha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",` +
		`"escalatedBaseSha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","escalationGeneration":1}` + " -->"}}, nil
}

func (p *selfHealStubProvider) UpdateWorkItem(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	p.updates = append(p.updates, req)
	return providers.WorkItem{}, nil
}
