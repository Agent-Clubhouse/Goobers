package main

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

const (
	// lineTolerance is how many lines a finding's location may sit from a
	// bug line and still overlap it.
	lineTolerance = 3

	matchLineOverlap = "line-overlap"
	matchJudge       = "judge"

	// classUnclassified groups findings that carry no finding class.
	classUnclassified = "unclassified"
)

// Case result statuses.
const (
	statusReviewed        = "reviewed"
	statusFailed          = "failed"
	statusBudgetExhausted = "budget-exhausted"
)

// judgeRequest asks a semantic judge whether one finding describes the bug a
// later fix addressed.
type judgeRequest struct {
	CaseID            string        `json:"caseId"`
	FixCommit         string        `json:"fixCommit"`
	FixSubject        string        `json:"fixSubject"`
	IntroducingCommit string        `json:"introducingCommit"`
	BugLines          []fileLines   `json:"bugLines"`
	Finding           apiv1.Finding `json:"finding"`
	// BudgetRemainingUSD is the most this decision may cost.
	BudgetRemainingUSD float64 `json:"budgetRemainingUsd"`
}

// judgeDecision is a judge's answer and what producing it cost; judge spend
// counts against the replay budget.
type judgeDecision struct {
	Match     bool    `json:"match"`
	Rationale string  `json:"rationale"`
	CostUSD   float64 `json:"costUsd,omitempty"`
}

// judge is an optional semantic matcher, consulted only for findings whose
// location does not overlap a bug line. Every decision is recorded in the
// report for audit.
type judge interface {
	Name() string
	Judge(ctx context.Context, request judgeRequest) (judgeDecision, error)
}

// commandJudge is an operator-supplied adapter command: it receives a
// judgeRequest as JSON on stdin and prints a judgeDecision as JSON. Like the
// reviewer, it runs in a scratch directory under workDir with an isolated
// home and no forge credentials.
type commandJudge struct {
	adapterCommand
}

func (judge commandJudge) Name() string {
	return "command:" + path.Base(strings.ReplaceAll(judge.argv[0], `\`, "/"))
}

func (judge commandJudge) Judge(ctx context.Context, request judgeRequest) (judgeDecision, error) {
	var decision judgeDecision
	err := judge.run(ctx, "", request, &decision)
	return decision, err
}

type findingResult struct {
	Class             string `json:"class"`
	Severity          string `json:"severity"`
	Location          string `json:"location,omitempty"`
	LearningSignature string `json:"learningSignature,omitempty"`
	Match             string `json:"match,omitempty"`
}

// judgeRecord is one judge decision, or a finding left unjudged because the
// budget ran out (SkippedForBudget), which then counts as not matching.
type judgeRecord struct {
	Finding          int     `json:"finding"`
	Judge            string  `json:"judge"`
	Match            bool    `json:"match"`
	Rationale        string  `json:"rationale,omitempty"`
	CostUSD          float64 `json:"costUsd,omitempty"`
	SkippedForBudget bool    `json:"skippedForBudget,omitempty"`
	// Error is set when the judge call failed; its cost still counts.
	Error string `json:"error,omitempty"`
}

// caseResult is one sampled case's outcome: the verdict the reviewer
// returned, how each finding was scored, and whether the bug was caught.
type caseResult struct {
	CaseID            string          `json:"caseId"`
	FixCommit         string          `json:"fixCommit"`
	IntroducingCommit string          `json:"introducingCommit"`
	Status            string          `json:"status"`
	Error             string          `json:"error,omitempty"`
	Verdict           *apiv1.Verdict  `json:"verdict,omitempty"`
	Findings          []findingResult `json:"findings,omitempty"`
	Caught            bool            `json:"caught"`
	CostUSD           float64         `json:"costUsd"`
	JudgeCostUSD      float64         `json:"judgeCostUsd,omitempty"`
	JudgeDecisions    []judgeRecord   `json:"judgeDecisions,omitempty"`
}

// matcher decides which findings match a case's bug: by location overlap
// with the lines the fix touched, then, if configured, by a semantic judge.
type matcher struct {
	judge judge
}

// score matches each finding to the case's bug, spending at most budget on
// judge calls. On a judge failure it returns the partial result, whose
// JudgeCostUSD is what the judge already spent.
func (m matcher) score(ctx context.Context, item datasetCase, reviewed reviewResult, budget float64) (caseResult, error) {
	verdict := reviewed.Verdict
	result := caseResult{
		CaseID: item.ID, FixCommit: item.FixCommit, IntroducingCommit: item.IntroducingCommit,
		Status: statusReviewed, Verdict: &verdict, Findings: []findingResult{}, CostUSD: round4(reviewed.CostUSD),
	}
	for index, finding := range verdict.Findings {
		scored := findingResult{
			Class: string(finding.Class), Severity: string(finding.Severity), Location: finding.Location,
			LearningSignature: finding.LearningSignature,
		}
		if scored.Class == "" {
			scored.Class = classUnclassified
		}
		switch {
		case overlapsBug(finding.Location, item.BugLines):
			scored.Match = matchLineOverlap
		case m.judge != nil && budget-result.JudgeCostUSD <= costEpsilon:
			result.JudgeDecisions = append(result.JudgeDecisions, judgeRecord{Finding: index, Judge: m.judge.Name(), SkippedForBudget: true})
		case m.judge != nil:
			matched, err := m.judgeFinding(ctx, &result, reviewed.CostUSD, index, judgeRequest{
				CaseID: item.ID, FixCommit: item.FixCommit, FixSubject: item.FixSubject,
				IntroducingCommit: item.IntroducingCommit, BugLines: item.BugLines, Finding: finding,
				BudgetRemainingUSD: budget - result.JudgeCostUSD,
			})
			if err != nil {
				return result, fmt.Errorf("judge finding %d: %w", index, err)
			}
			if matched {
				scored.Match = matchJudge
			}
		}
		result.Caught = result.Caught || scored.Match != ""
		result.Findings = append(result.Findings, scored)
	}
	return result, nil
}

// judgeFinding runs one judge call and records it on result. A failed call
// still spent what it reports, so its cost is recorded and checked against
// the remaining budget like any other; only its verdict is discarded.
func (m matcher) judgeFinding(ctx context.Context, result *caseResult, reviewedCost float64, index int, request judgeRequest) (bool, error) {
	decision, err := m.judge.Judge(ctx, request)
	if !validCost(decision.CostUSD) {
		return false, errors.Join(err, fmt.Errorf("judge reported invalid cost %v", decision.CostUSD))
	}
	result.JudgeCostUSD += decision.CostUSD
	result.CostUSD = round4(reviewedCost + result.JudgeCostUSD)
	record := judgeRecord{Finding: index, Judge: m.judge.Name(), CostUSD: decision.CostUSD}
	if err == nil {
		record.Match, record.Rationale = decision.Match, decision.Rationale
	} else {
		record.Error = err.Error()
	}
	result.JudgeDecisions = append(result.JudgeDecisions, record)
	if decision.CostUSD > request.BudgetRemainingUSD+costEpsilon {
		return false, errors.Join(err, fmt.Errorf("judge reported $%.4f with $%.4f left: %w", decision.CostUSD, request.BudgetRemainingUSD, errBudgetOverrun))
	}
	return record.Match, err
}

var (
	colonLocation  = regexp.MustCompile(`^(.+?):(\d+)(?:[-:](\d+))?$`)
	anchorLocation = regexp.MustCompile(`^(.+?)#L(\d+)(?:-L?(\d+))?$`)
)

// parseLocation reads a finding location such as "path", "path:42",
// "path:42-50", or "path#L42-L50". Start is 0 when no line is given.
func parseLocation(location string) (file string, start, end int) {
	location = strings.TrimSpace(location)
	if cut := strings.IndexAny(location, " \t"); cut >= 0 {
		location = location[:cut]
	}
	match := anchorLocation.FindStringSubmatch(location)
	if match == nil {
		match = colonLocation.FindStringSubmatch(location)
	}
	if match == nil {
		return normalizePath(location), 0, 0
	}
	start, _ = strconv.Atoi(match[2])
	end, _ = strconv.Atoi(match[3])
	if end < start {
		end = start
	}
	return normalizePath(match[1]), start, end
}

func normalizePath(file string) string {
	file = strings.ReplaceAll(file, `\`, "/")
	for _, prefix := range []string{"./", "a/", "b/"} {
		file = strings.TrimPrefix(file, prefix)
	}
	return file
}

// overlapsBug reports whether a location names a bug file and a line within
// lineTolerance of one of its bug lines. A location without a line never
// overlaps: naming the file alone is left to the judge.
func overlapsBug(location string, bugs []fileLines) bool {
	file, start, end := parseLocation(location)
	if start == 0 {
		return false
	}
	for _, bug := range bugs {
		if normalizePath(bug.Path) != file {
			continue
		}
		for _, line := range bug.Lines {
			if line >= start-lineTolerance && line <= end+lineTolerance {
				return true
			}
		}
	}
	return false
}
