package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strconv"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// verdictLabel maps a #358 Verdict's Decision to the design doc's label
// contract (§3): pass -> eligible to merge, needs-changes -> selected by
// pr-remediation, fail -> a human must look (§4 D2: fail is never burned on
// remediation budget, unlike needs-changes).
func verdictLabel(decision apiv1.VerdictDecision) string {
	switch decision {
	case apiv1.VerdictPass:
		return "goobers:merge-ready"
	case apiv1.VerdictFail:
		return "goobers:merge-escalated"
	default:
		return "goobers:needs-remediation"
	}
}

// runApplyVerdict implements `goobers apply-verdict` (issue #359): reads the
// holistic review gate's Verdict back from this run's own journal (the gate
// already records it as an artifact via internal/gate's recordVerdict — no
// new plumbing), re-checks its SHA pin against the PR's CURRENT head/base
// before acting (design doc §6 D6: a verdict computed against a state that
// no longer exists is void, not actionable), then posts the prose-projection
// comment and applies the decision label.
func runApplyVerdict(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("apply-verdict", flag.ContinueOnError)
	fs.SetOutput(stderr)
	gateName := fs.String("gate", "review", "the gate name whose verdict to apply")
	fs.Usage = func() {
		pf(stderr, "Usage: goobers apply-verdict [--gate name] [path]\n\n"+
			"Read the holistic review gate's Verdict from this run's own journal,\n"+
			"re-check its SHA pin against the PR's current head/base, and — if\n"+
			"still valid — post the verdict as a PR comment and apply the\n"+
			"decision label (merge-ready/needs-remediation/merge-escalated). A\n"+
			"stale SHA pin voids the verdict: no comment, no label, exit 0 (this\n"+
			"cycle's work is simply moot, not an error — merge-review re-reviews\n"+
			"next tick). Requires selectedNumber (Task.InputsFrom pr-select's\n"+
			"number output). Exit codes: 0 = applied (or voided), 1 = business\n"+
			"error, 2 = usage/IO error.\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	pathArg := ""
	if fs.NArg() == 1 {
		pathArg = fs.Arg(0)
	}
	root := providerStageRoot(pathArg)

	selectedNumberStr := providerInput("selectedNumber", "")
	if selectedNumberStr == "" {
		pf(stderr, "error: selectedNumber is required (inputsFrom pr-select's number output)\n")
		return 1
	}
	selectedNumber, err := strconv.Atoi(selectedNumberStr)
	if err != nil {
		pf(stderr, "error: invalid selectedNumber %q: %v\n", selectedNumberStr, err)
		return 1
	}

	runID, _, err := providerRunContext()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	l := layoutFor(root)
	verdict, err := readLatestGateVerdict(l.RunsDir(), runID, *gateName)
	if err != nil {
		pf(stderr, "error: read %s verdict from journal: %v\n", *gateName, err)
		return 1
	}
	if verdict == nil {
		pf(stderr, "error: no %s gate.evaluated event with a verdict found in this run's journal\n", *gateName)
		return 1
	}

	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	token, err := providerToken(capability.GitHubPRWrite)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	provider := newGitHubProvider(token)

	base := providerInput("base", "main")
	headPrefix := providerInput("headPrefix", "goobers/")
	ctx := context.Background()
	prs, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: repo, Base: base, HeadPrefix: headPrefix,
	})
	if err != nil {
		pf(stderr, "error: list pull requests: %v\n", err)
		return 1
	}
	var current *providers.PullRequestSummary
	for i := range prs {
		if prs[i].Number == selectedNumber {
			current = &prs[i]
			break
		}
	}
	if current == nil {
		pln(stdout, "PR is no longer open (merged/closed since selection) — verdict moot, nothing to apply")
		return 0
	}

	// D6: the verdict is void if the PR has moved since it was computed —
	// either new commits landed (headSha changed) or the base advanced
	// (baseSha changed). Acting on a stale verdict would label/comment
	// against a diff that no longer exists.
	if verdict.HeadSHA != "" && verdict.HeadSHA != current.HeadSHA {
		pf(stdout, "verdict void: PR #%d's head moved (%s -> %s) since review — skipping, will re-review next cycle\n", selectedNumber, verdict.HeadSHA, current.HeadSHA)
		return 0
	}
	if verdict.BaseSHA != "" && verdict.BaseSHA != current.BaseSHA {
		pf(stdout, "verdict void: PR #%d's base moved (%s -> %s) since review — skipping, will re-review next cycle\n", selectedNumber, verdict.BaseSHA, current.BaseSHA)
		return 0
	}

	label := verdictLabel(verdict.Decision)
	comment := renderVerdictComment(*verdict)
	if _, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository: repo,
		ID:         strconv.Itoa(selectedNumber),
		AddLabels:  []string{label},
		Comment:    comment,
	}); err != nil {
		pf(stderr, "error: apply verdict to PR #%d: %v\n", selectedNumber, err)
		return 1
	}

	pf(stdout, "applied %s to PR #%d (%s)\n", label, selectedNumber, verdict.Decision)
	return 0
}

// readLatestGateVerdict reads runID's own journal and returns the Verdict
// artifact of the LAST gate.evaluated event named gateName (last, not
// first, in case a repass re-evaluated it) — nil, nil if no such event
// exists yet.
func readLatestGateVerdict(runsDir, runID, gateName string) (*apiv1.Verdict, error) {
	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		return nil, err
	}
	events, err := rd.Events()
	if err != nil {
		return nil, err
	}
	var ref *journal.Ref
	for i := range events {
		e := &events[i]
		if e.Type == journal.EventGateEvaluated && e.Gate == gateName && e.Ref != nil {
			ref = e.Ref
		}
	}
	if ref == nil {
		return nil, nil
	}
	data, err := rd.ArtifactBytes(*ref)
	if err != nil {
		return nil, fmt.Errorf("read verdict artifact: %w", err)
	}
	var v apiv1.Verdict
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("unmarshal verdict artifact: %w", err)
	}
	return &v, nil
}

// renderVerdictComment is the prose PR comment — a human-readable
// projection of the same Verdict artifact (design doc §4: "one source of
// truth, so comment and fix cannot drift"), never a separately-authored
// message.
func renderVerdictComment(v apiv1.Verdict) string {
	s := fmt.Sprintf("**merge-review verdict: %s**\n\n%s", v.Decision, v.Summary)
	if v.Rationale != "" {
		s += "\n\n" + v.Rationale
	}
	for _, f := range v.Findings {
		line := fmt.Sprintf("\n- [%s] %s", f.Severity, f.Message)
		if f.Class != "" {
			line = fmt.Sprintf("\n- [%s/%s] %s", f.Severity, f.Class, f.Message)
		}
		if f.Location != "" {
			line += " (" + f.Location + ")"
		}
		s += line
	}
	return s
}
