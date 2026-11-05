package main

import (
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/goobers/goobers/internal/creditgraph"
)

const (
	ablationReportSchemaVersion = "goobers.dev/backprop/evidence-ablation-report/v1"
	// minimumAffectedCases is the predeclared sample size below which a
	// hypothesis criterion reports insufficient-sample instead of a verdict.
	minimumAffectedCases = 10
	// wilsonZ is the normal quantile of a two-sided 95% Wilson interval.
	wilsonZ = 1.959963984540054
	// significanceLevel is the predeclared exact McNemar threshold.
	significanceLevel = 0.05
)

// criterionVerdict is the outcome of one predeclared success criterion.
type criterionVerdict string

const (
	verdictPass               criterionVerdict = "pass"
	verdictFail               criterionVerdict = "fail"
	verdictInsufficientSample criterionVerdict = "insufficient-sample"
)

// rate is a proportion with its 95% Wilson interval. With no samples every
// field is zero.
type rate struct {
	Count int     `json:"count"`
	Total int     `json:"total"`
	Rate  float64 `json:"rate"`
	Lower float64 `json:"lower"`
	Upper float64 `json:"upper"`
}

type granularityRate struct {
	Granularity granularity `json:"granularity"`
	rate
}

// pairedComparison compares one fidelity level with full fidelity case by
// case at one granularity. Lost cases were correct at full fidelity and are
// not at this level; Gained the reverse. McNemarP is the exact two-sided
// McNemar p-value over the discordant cases.
type pairedComparison struct {
	Granularity granularity `json:"granularity"`
	BothCorrect int         `json:"bothCorrect"`
	Lost        int         `json:"lost"`
	Gained      int         `json:"gained"`
	BothWrong   int         `json:"bothWrong"`
	McNemarP    float64     `json:"mcnemarExactP"`
}

// confidenceShift counts how the top cause's confidence moved from full
// fidelity on the cases an ablation changed. An abstention's confidence is
// confidence in refusing to attribute, not in a cause, so it is not compared:
// abstaining cases are counted in Abstained instead.
type confidenceShift struct {
	Lower     int `json:"lower"`
	Unchanged int `json:"unchanged"`
	Higher    int `json:"higher"`
	Abstained int `json:"abstained"`
}

// citationCheck counts the graph nodes causes cite (the blamed node and every
// node-ID evidence entry) and how many resolve in the graph built at the same
// fidelity. A cited tool result must also carry a recorded outcome, since
// that outcome is what the citation asserts.
type citationCheck struct {
	Total int `json:"total"`
	Valid int `json:"valid"`
}

// ablationCaseResult is one case attributed at one fidelity level. It carries
// IDs, classes, and counts only, never transcript content.
type ablationCaseResult struct {
	caseResult
	Adapters  []string       `json:"adapters,omitempty"`
	Change    evidenceChange `json:"change"`
	Affected  bool           `json:"affected"`
	Abstained bool           `json:"abstained"`
	// Unsupported is a committed (non-unknown) top cause that names or cites
	// a node the graph at this fidelity does not support.
	Unsupported bool          `json:"unsupported"`
	Citations   citationCheck `json:"citations"`
}

// ablationVariant summarizes one fidelity level over the whole set.
// Affected-case fields cover only the cases whose evidence the level changed.
type ablationVariant struct {
	Level                  fidelityLevel      `json:"level"`
	CaseCount              int                `json:"caseCount"`
	AffectedCases          int                `json:"affectedCases"`
	Accuracy               []granularityRate  `json:"accuracy"`
	AffectedAccuracy       []granularityRate  `json:"affectedAccuracy"`
	PairedWithFull         []pairedComparison `json:"pairedWithFull"`
	Abstention             rate               `json:"abstention"`
	AffectedAbstention     rate               `json:"affectedAbstention"`
	UnsupportedAttribution rate               `json:"unsupportedAttribution"`
	CitationValidity       rate               `json:"citationValidity"`
	// Misattributions counts affected cases whose top cause commits to a
	// class other than the one full fidelity committed to (or abstained on).
	Misattributions        int                  `json:"misattributions"`
	ConfidenceShift        confidenceShift      `json:"confidenceShift"`
	DomainBrierScore       float64              `json:"domainBrierScore"`
	Coverage               []adapterCoverage    `json:"coverage"`
	CasesWithoutTranscript int                  `json:"casesWithoutTranscript"`
	Results                []ablationCaseResult `json:"results"`
}

// ablationCriterion is one predeclared success criterion and its verdict.
type ablationCriterion struct {
	ID        string           `json:"id"`
	Statement string           `json:"statement"`
	Verdict   criterionVerdict `json:"verdict"`
	Detail    string           `json:"detail"`
}

// ablationReport is the paired evidence-fidelity ablation report (#7317).
type ablationReport struct {
	Schema                     string              `json:"schema"`
	EvalSetVersion             string              `json:"evalSetVersion"`
	EvalSetDigest              string              `json:"evalSetDigest"`
	CaseCount                  int                 `json:"caseCount"`
	MinimumAffectedCases       int                 `json:"minimumAffectedCases"`
	UnrecordedToolObservations []string            `json:"unrecordedToolObservations"`
	Variants                   []ablationVariant   `json:"variants"`
	Criteria                   []ablationCriterion `json:"criteria"`
}

// runAblation attributes every case of the set at every fidelity level and
// compares each level with full fidelity case by case.
func runAblation(set evalSet) (ablationReport, error) {
	plain, err := runEval(set)
	if err != nil {
		return ablationReport{}, err
	}
	cases := sortedCases(set.Cases)
	report := ablationReport{
		Schema:                     ablationReportSchemaVersion,
		EvalSetVersion:             set.Version,
		EvalSetDigest:              set.digest(),
		CaseCount:                  len(cases),
		MinimumAffectedCases:       minimumAffectedCases,
		UnrecordedToolObservations: append([]string(nil), unrecordedToolObservations...),
	}
	var full []ablationCaseResult
	for _, level := range fidelityLevels {
		results, collector, err := attributeAtLevel(cases, level)
		if err != nil {
			return ablationReport{}, err
		}
		if level == fidelityFull {
			full = results
		}
		report.Variants = append(report.Variants, summarizeVariant(level, results, full, collector))
	}
	report.Criteria = evaluateCriteria(report, plain)
	return report, nil
}

func attributeAtLevel(cases []evalCase, level fidelityLevel) ([]ablationCaseResult, *coverageCollector, error) {
	collector := newCoverageCollector()
	results := make([]ablationCaseResult, 0, len(cases))
	for _, evalCase := range cases {
		input, change := ablate(evalCase.Input, level)
		adapters := collector.add(input)
		graph, attribution, err := attribute(input)
		if err != nil {
			return nil, nil, fmt.Errorf("evidence ablation %s case %q: %w", level, evalCase.ID, err)
		}
		result := ablationCaseResult{
			caseResult: score(evalCase, predictionOf(input, attribution)),
			Adapters:   adapters, Change: change, Affected: change.any(),
		}
		result.Abstained = !result.Predicted.Attributed || result.Predicted.Class == creditgraph.ClassUnknown
		for _, cause := range attribution.Causes {
			check := checkCitations(graph, cause)
			result.Citations.Total += check.Total
			result.Citations.Valid += check.Valid
		}
		if top, ok := topCause(attribution.Causes); ok && top.Class != creditgraph.ClassUnknown {
			check := checkCitations(graph, top)
			result.Unsupported = check.Valid < check.Total
		}
		results = append(results, result)
	}
	return results, collector, nil
}

func checkCitations(graph *creditgraph.Graph, cause creditgraph.CauseFinding) citationCheck {
	check := citationCheck{}
	for _, id := range append([]string{cause.NodeID}, nodeCitations(cause.Evidence)...) {
		check.Total++
		if citationResolves(graph, id) {
			check.Valid++
		}
	}
	return check
}

// nodeCitations are the evidence entries that name graph nodes: everything
// but intervention notes and agent-claim citations.
func nodeCitations(evidence []string) []string {
	var ids []string
	for _, entry := range evidence {
		if strings.HasPrefix(entry, "intervention:") || strings.HasPrefix(entry, creditgraph.ClaimEvidencePrefix) {
			continue
		}
		ids = append(ids, entry)
	}
	return ids
}

func citationResolves(graph *creditgraph.Graph, id string) bool {
	node, ok := graph.Node(id)
	if !ok {
		return false
	}
	if node.Kind == creditgraph.KindToolResult {
		_, recorded := node.Attributes["success"]
		return recorded
	}
	return true
}

func summarizeVariant(level fidelityLevel, results, full []ablationCaseResult, collector *coverageCollector) ablationVariant {
	variant := ablationVariant{
		Level: level, CaseCount: len(results),
		Coverage: collector.coverage(), CasesWithoutTranscript: collector.casesWithoutTranscript,
		Results: results,
	}
	var affected []ablationCaseResult
	plain := make([]caseResult, 0, len(results))
	abstained, affectedAbstained, committed, unsupported := 0, 0, 0, 0
	for index, result := range results {
		plain = append(plain, result.caseResult)
		variant.CitationValidity.Count += result.Citations.Valid
		variant.CitationValidity.Total += result.Citations.Total
		if result.Abstained {
			abstained++
		} else {
			committed++
		}
		if result.Unsupported {
			unsupported++
		}
		if !result.Affected {
			continue
		}
		affected = append(affected, result)
		if result.Abstained {
			affectedAbstained++
		}
		variant.recordShift(result, full[index])
	}
	variant.AffectedCases = len(affected)
	variant.Accuracy = accuracyRates(results)
	variant.AffectedAccuracy = accuracyRates(affected)
	variant.PairedWithFull = pairedComparisons(results, full)
	variant.Abstention = rateOf(abstained, len(results))
	variant.AffectedAbstention = rateOf(affectedAbstained, len(affected))
	variant.UnsupportedAttribution = rateOf(unsupported, committed)
	variant.CitationValidity = rateOf(variant.CitationValidity.Count, variant.CitationValidity.Total)
	variant.DomainBrierScore = calibrate(plain, granularityDomain).BrierScore
	return variant
}

// recordShift compares one affected case with its full-fidelity result.
func (variant *ablationVariant) recordShift(result, full ablationCaseResult) {
	if result.Abstained {
		variant.ConfidenceShift.Abstained++
		return
	}
	if result.Predicted.Class != full.Predicted.Class {
		variant.Misattributions++
	}
	switch {
	case full.Abstained:
		// Committing where full fidelity abstained is a gain in confidence
		// in a cause, whatever the refusal's own number was.
		variant.ConfidenceShift.Higher++
	case result.Predicted.Confidence < full.Predicted.Confidence:
		variant.ConfidenceShift.Lower++
	case result.Predicted.Confidence > full.Predicted.Confidence:
		variant.ConfidenceShift.Higher++
	default:
		variant.ConfidenceShift.Unchanged++
	}
}

func accuracyRates(results []ablationCaseResult) []granularityRate {
	rates := make([]granularityRate, 0, len(granularities))
	for _, level := range granularities {
		correct := 0
		for _, result := range results {
			if result.hit(level) {
				correct++
			}
		}
		rates = append(rates, granularityRate{Granularity: level, rate: rateOf(correct, len(results))})
	}
	return rates
}

func pairedComparisons(results, full []ablationCaseResult) []pairedComparison {
	comparisons := make([]pairedComparison, 0, len(granularities))
	for _, level := range granularities {
		comparison := pairedComparison{Granularity: level}
		for index, result := range results {
			was, is := full[index].hit(level), result.hit(level)
			switch {
			case was && is:
				comparison.BothCorrect++
			case was:
				comparison.Lost++
			case is:
				comparison.Gained++
			default:
				comparison.BothWrong++
			}
		}
		comparison.McNemarP = round4(mcnemarExactP(comparison.Lost, comparison.Gained))
		comparisons = append(comparisons, comparison)
	}
	return comparisons
}

// rateOf is count/total with its 95% Wilson score interval.
func rateOf(count, total int) rate {
	if total <= 0 {
		return rate{}
	}
	n := float64(total)
	p := float64(count) / n
	z2 := wilsonZ * wilsonZ
	center := (p + z2/(2*n)) / (1 + z2/n)
	half := wilsonZ / (1 + z2/n) * math.Sqrt(p*(1-p)/n+z2/(4*n*n))
	return rate{
		Count: count, Total: total, Rate: round4(p),
		Lower: round4(math.Max(0, center-half)), Upper: round4(math.Min(1, center+half)),
	}
}

// mcnemarExactP is the exact two-sided McNemar p-value for b and c
// discordant pairs: a two-sided binomial test of min(b,c) at p=0.5.
func mcnemarExactP(b, c int) float64 {
	n := b + c
	if n == 0 {
		return 1
	}
	tail := 0.0
	for i := 0; i <= min(b, c); i++ {
		tail += math.Exp(logChoose(n, i) - float64(n)*math.Ln2)
	}
	return math.Min(1, 2*tail)
}

func logChoose(n, k int) float64 {
	nl, _ := math.Lgamma(float64(n + 1))
	kl, _ := math.Lgamma(float64(k + 1))
	rl, _ := math.Lgamma(float64(n - k + 1))
	return nl - kl - rl
}

// evaluateCriteria applies the predeclared success criteria (see
// docs/design/credit-graph.md, "Evidence fidelity ablation"). Safety
// criteria fail on a single violation at any sample size; the hypothesis
// criterion needs minimumAffectedCases affected cases before it reports a
// verdict.
func evaluateCriteria(report ablationReport, plain evalReport) []ablationCriterion {
	return []ablationCriterion{
		fullMatchesEvalCriterion(report, plain),
		ablatedCriterion(report, false, "no-silent-misattribution",
			"under every ablation, each affected case abstains or keeps its full-fidelity class",
			func(variant ablationVariant) (bool, string) {
				return variant.Misattributions == 0, fmt.Sprintf("%d misattribution(s)", variant.Misattributions)
			}),
		ablatedCriterion(report, false, "no-confidence-gain",
			"under every ablation, no affected case that still commits to a cause gains top-cause confidence",
			func(variant ablationVariant) (bool, string) {
				return variant.ConfidenceShift.Higher == 0, fmt.Sprintf("%d higher", variant.ConfidenceShift.Higher)
			}),
		ablatedCriterion(report, true, "citations-supported",
			"at every fidelity, every cited node resolves and no committed cause is unsupported",
			func(variant ablationVariant) (bool, string) {
				ok := variant.CitationValidity.Count == variant.CitationValidity.Total && variant.UnsupportedAttribution.Count == 0
				return ok, fmt.Sprintf("citations %d/%d valid, %d unsupported", variant.CitationValidity.Count,
					variant.CitationValidity.Total, variant.UnsupportedAttribution.Count)
			}),
		outcomeEvidenceCriterion(report),
	}
}

func fullMatchesEvalCriterion(report ablationReport, plain evalReport) ablationCriterion {
	criterion := ablationCriterion{
		ID:        "full-matches-eval",
		Statement: "full-fidelity accuracy equals the attribution eval's accuracy at every granularity",
		Verdict:   verdictPass,
	}
	full := report.Variants[0]
	for index, entry := range plain.Accuracy {
		got := full.Accuracy[index]
		if got.Granularity != entry.Granularity || got.Count != entry.Correct || got.Total != entry.Total {
			criterion.Verdict = verdictFail
			criterion.Detail = fmt.Sprintf("%s: %d/%d, eval %d/%d", entry.Granularity, got.Count, got.Total, entry.Correct, entry.Total)
			return criterion
		}
	}
	criterion.Detail = fmt.Sprintf("%d cases", full.CaseCount)
	return criterion
}

// ablatedCriterion holds every ablated level, and full fidelity too when
// includeFull is set, to check; the detail names each level's measurement.
func ablatedCriterion(report ablationReport, includeFull bool, id, statement string, check func(ablationVariant) (bool, string)) ablationCriterion {
	criterion := ablationCriterion{ID: id, Statement: statement, Verdict: verdictPass}
	var details []string
	for _, variant := range report.Variants {
		if variant.Level == fidelityFull && !includeFull {
			continue
		}
		ok, detail := check(variant)
		if !ok {
			criterion.Verdict = verdictFail
		}
		details = append(details, string(variant.Level)+": "+detail)
	}
	criterion.Detail = strings.Join(details, "; ")
	return criterion
}

func outcomeEvidenceCriterion(report ablationReport) ablationCriterion {
	criterion := ablationCriterion{
		ID: "outcome-evidence-useful",
		Statement: fmt.Sprintf("removing tool outcomes loses more class-correct cases than it gains (exact McNemar p < %.2f) over at least %d affected cases",
			significanceLevel, minimumAffectedCases),
	}
	variant, ok := report.variant(fidelityOutcomeAblated)
	if !ok {
		criterion.Verdict = verdictInsufficientSample
		return criterion
	}
	paired := variant.paired(granularityClass)
	criterion.Detail = fmt.Sprintf("%d affected case(s): class lost %d, gained %d, p=%.4f",
		variant.AffectedCases, paired.Lost, paired.Gained, paired.McNemarP)
	switch {
	case variant.AffectedCases < minimumAffectedCases:
		criterion.Verdict = verdictInsufficientSample
	case paired.Lost > paired.Gained && paired.McNemarP < significanceLevel:
		criterion.Verdict = verdictPass
	default:
		criterion.Verdict = verdictFail
	}
	return criterion
}

func (variant ablationVariant) paired(level granularity) pairedComparison {
	for _, comparison := range variant.PairedWithFull {
		if comparison.Granularity == level {
			return comparison
		}
	}
	return pairedComparison{Granularity: level}
}

func (variant ablationVariant) accuracyAt(level granularity) granularityRate {
	for _, entry := range variant.Accuracy {
		if entry.Granularity == level {
			return entry
		}
	}
	return granularityRate{Granularity: level}
}

func (report ablationReport) variant(level fidelityLevel) (ablationVariant, bool) {
	for _, variant := range report.Variants {
		if variant.Level == level {
			return variant, true
		}
	}
	return ablationVariant{}, false
}

func printAblationSummary(out io.Writer, report ablationReport) {
	_, _ = fmt.Fprintf(out, "evidence ablation %s (%d cases, %s)\n", report.EvalSetVersion, report.CaseCount, report.EvalSetDigest)
	for _, variant := range report.Variants {
		class := variant.accuracyAt(granularityClass)
		_, _ = fmt.Fprintf(out, "  %-18s affected=%d class=%d/%d [%.2f,%.2f] abstain(affected)=%d/%d unsupported=%d/%d citations=%d/%d misattributed=%d\n",
			variant.Level, variant.AffectedCases, class.Count, class.Total, class.Lower, class.Upper,
			variant.AffectedAbstention.Count, variant.AffectedAbstention.Total,
			variant.UnsupportedAttribution.Count, variant.UnsupportedAttribution.Total,
			variant.CitationValidity.Count, variant.CitationValidity.Total, variant.Misattributions)
	}
	for _, criterion := range report.Criteria {
		_, _ = fmt.Fprintf(out, "  criterion %-24s %s: %s\n", criterion.ID, criterion.Verdict, criterion.Detail)
	}
}
