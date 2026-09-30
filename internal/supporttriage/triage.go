// Package supporttriage classifies redacted diagnostics bundles before a
// product issue is filed.
package supporttriage

import (
	"sort"
	"strings"

	"github.com/goobers/goobers/internal/diagnostics"
	"github.com/goobers/goobers/internal/supportmatrix"
)

// Schema identifies the support-triage result contract.
const Schema = "goobers.dev/support-triage/v1"

// Disposition is the explicit support-case routing outcome.
type Disposition string

const (
	// DispositionWorkflowConfiguration routes reports to workflow/config fixes.
	DispositionWorkflowConfiguration Disposition = "workflow_configuration"
	// DispositionHarnessAuthentication routes reports to missing or invalid harness credentials.
	DispositionHarnessAuthentication Disposition = "harness_authentication"
	// DispositionLocalEnvironment routes reports to local runner environment fixes.
	DispositionLocalEnvironment Disposition = "local_environment"
	// DispositionExternalDependency routes reports to external provider ownership.
	DispositionExternalDependency Disposition = "external_dependency"
	// DispositionTransientFailure routes reports to retry or repeated-failure collection.
	DispositionTransientFailure Disposition = "transient_failure"
	// DispositionUnsupportedVersion routes reports to a supported Goobers version.
	DispositionUnsupportedVersion Disposition = "unsupported_version"
	// DispositionDuplicateOrKnown routes reports to an existing known issue.
	DispositionDuplicateOrKnown Disposition = "duplicate_or_known"
	// DispositionNotReproduced keeps reports in support until reproduction exists.
	DispositionNotReproduced Disposition = "not_reproduced"
	// DispositionInsufficientEvidence is the fail-closed ambiguous outcome.
	DispositionInsufficientEvidence Disposition = "insufficient_evidence"
	// DispositionGoobersDefectCandidate marks positive product-owned evidence.
	DispositionGoobersDefectCandidate Disposition = "goobers_defect_candidate"
)

// Evidence is one sanitized structured fact that contributed to the verdict.
type Evidence struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject,omitempty"`
	Detail  string `json:"detail"`
}

// Result is the versioned support-triage output consumed by intake surfaces.
type Result struct {
	Schema      string      `json:"schema"`
	Disposition Disposition `json:"disposition"`
	Confidence  string      `json:"confidence"`
	NextAction  string      `json:"nextAction"`
	Evidence    []Evidence  `json:"evidence,omitempty"`
	Case        CaseSummary `json:"case"`
	Cluster     ClusterInfo `json:"cluster"`
}

// CaseSummary identifies the report being classified without retaining raw bodies.
type CaseSummary struct {
	RunIDs      []string `json:"runIds,omitempty"`
	Workflows   []string `json:"workflows,omitempty"`
	Gaggles     []string `json:"gaggles,omitempty"`
	Binary      string   `json:"binaryVersion,omitempty"`
	Platform    string   `json:"platform,omitempty"`
	Fingerprint string   `json:"fingerprint"`
}

// ClusterInfo groups matching reports across versions, platforms, and harnesses.
type ClusterInfo struct {
	Fingerprint          string   `json:"fingerprint"`
	OccurrenceCount      int      `json:"occurrenceCount"`
	AffectedVersions     []string `json:"affectedVersions,omitempty"`
	AffectedPlatforms    []string `json:"affectedPlatforms,omitempty"`
	AffectedHarnesses    []string `json:"affectedHarnesses,omitempty"`
	KnownIssue           string   `json:"knownIssue,omitempty"`
	ReproductionAttempts []string `json:"reproductionAttempts,omitempty"`
}

type signal struct {
	disposition Disposition
	evidence    Evidence
}

// Classify derives a fail-closed support triage result from a redacted bundle.
func Classify(bundle diagnostics.Bundle) Result {
	signals := collectSignals(bundle)
	result := Result{
		Schema:     Schema,
		Case:       summarizeCase(bundle),
		Cluster:    summarizeCluster(bundle),
		Confidence: "low",
	}
	result.Cluster.Fingerprint = result.Case.Fingerprint
	if result.Cluster.OccurrenceCount == 0 {
		result.Cluster.OccurrenceCount = len(bundle.Runs)
	}
	if len(bundle.Notes) > 0 {
		result.Evidence = append(result.Evidence, Evidence{
			Kind:   "collection_gap",
			Detail: "one or more diagnostic sources could not be collected; collection gaps are not root-cause evidence",
		})
	}
	if hasContradiction(signals) {
		result.Disposition = DispositionInsufficientEvidence
		result.NextAction = "Collect a complete diagnostics bundle and resolve contradictory support signals before filing a product issue."
		result.Evidence = append(result.Evidence, evidenceFromSignals(signals)...)
		return result
	}
	positive := uniqueDispositions(signals)
	if len(positive) == 1 {
		result.Disposition = positive[0]
		result.Confidence = confidenceFor(result.Disposition)
		result.NextAction = nextActionFor(result.Disposition)
		result.Evidence = append(result.Evidence, evidenceFromSignals(signals)...)
		return result
	}
	if len(positive) > 1 {
		result.Disposition = DispositionInsufficientEvidence
		result.NextAction = "Resolve competing support signals before routing or filing a product issue."
		result.Evidence = append(result.Evidence, evidenceFromSignals(signals)...)
		return result
	}
	result.Disposition = DispositionInsufficientEvidence
	result.NextAction = "Request reproduction steps or a fresh diagnostics bundle with the failing run selected."
	return result
}

func collectSignals(bundle diagnostics.Bundle) []signal {
	var signals []signal
	if versionUnsupported(bundle.Binary.Version) {
		signals = append(signals, signal{
			disposition: DispositionUnsupportedVersion,
			evidence:    Evidence{Kind: "binary_version", Subject: bundle.Binary.Version, Detail: "binary version is outside the current supported Goobers release window"},
		})
	}
	for _, issue := range bundle.Instance.ConfigIssues {
		if !strings.Contains(" "+strings.ToUpper(issue), " ERROR ") {
			continue
		}
		signals = append(signals, signal{
			disposition: DispositionWorkflowConfiguration,
			evidence:    Evidence{Kind: "config_validation", Detail: issue},
		})
	}
	for _, run := range bundle.Runs {
		for _, decision := range run.Decisions {
			lower := lowerJoined(decision.Reason)
			if containsAny(lower, "known issue", "duplicate of") {
				signals = append(signals, signal{disposition: DispositionDuplicateOrKnown, evidence: Evidence{Kind: "selector_decision", Subject: run.RunID, Detail: decision.Reason}})
			}
			if containsAny(lower, "not reproduced", "could not reproduce", "cannot reproduce") {
				signals = append(signals, signal{disposition: DispositionNotReproduced, evidence: Evidence{Kind: "selector_decision", Subject: run.RunID, Detail: decision.Reason}})
			}
			if containsAny(lower, "insufficient evidence", "ambiguous", "cannot determine") {
				signals = append(signals, signal{disposition: DispositionInsufficientEvidence, evidence: Evidence{Kind: "selector_decision", Subject: run.RunID, Detail: decision.Reason}})
			}
		}
		for _, err := range runErrors(run) {
			signals = append(signals, classifyError(run.RunID, err, canPromoteProductCandidate(bundle))...)
		}
	}
	return signals
}

func classifyError(runID string, err diagnostics.ErrorInfo, allowProductCandidate bool) []signal {
	code := lowerJoined(err.Code)
	message := lowerJoined(err.Message)
	text := strings.TrimSpace(code + " " + message)
	if text == "" {
		return nil
	}
	evidence := Evidence{Kind: "error", Subject: runID, Detail: strings.TrimSpace(err.Code + ": " + err.Message)}
	switch {
	case containsAny(text, "insufficient_evidence", "insufficient evidence", "ambiguous", "contradict"):
		return []signal{{disposition: DispositionInsufficientEvidence, evidence: evidence}}
	case containsAny(text, "panic", "should never happen", "internal invariant") && !isGoobersOwnedDefect(text):
		return nil
	case containsAny(text, "config", "validation", "schema", "workflow"):
		return []signal{{disposition: DispositionWorkflowConfiguration, evidence: evidence}}
	case containsAny(text, "auth", "credential", "unauthorized", "forbidden", "401", "403"):
		return []signal{{disposition: DispositionHarnessAuthentication, evidence: evidence}}
	case containsAny(text, "enoent", "not on path", "permission denied", "disk", "local environment", "workspace"):
		return []signal{{disposition: DispositionLocalEnvironment, evidence: evidence}}
	case containsAny(text, "rate limit", "provider", "external", "outage", "service unavailable", "bad gateway"):
		return []signal{{disposition: DispositionExternalDependency, evidence: evidence}}
	case containsAny(text, "timeout", "temporarily", "transient", "try again", "connection reset"):
		return []signal{{disposition: DispositionTransientFailure, evidence: evidence}}
	case isGoobersOwnedDefect(text):
		if !allowProductCandidate {
			return []signal{{
				disposition: DispositionInsufficientEvidence,
				evidence: Evidence{
					Kind:    "support_context",
					Subject: runID,
					Detail:  "candidate-looking Goobers evidence requires a supported binary version and complete diagnostics",
				},
			}}
		}
		return []signal{{disposition: DispositionGoobersDefectCandidate, evidence: evidence}}
	default:
		return nil
	}
}

func runErrors(run diagnostics.RunInfo) []diagnostics.ErrorInfo {
	var out []diagnostics.ErrorInfo
	if run.DecisiveError != nil {
		out = append(out, *run.DecisiveError)
	}
	for _, stage := range run.Stages {
		if stage.Error != nil {
			out = append(out, *stage.Error)
		}
	}
	return out
}

func isGoobersOwnedDefect(text string) bool {
	if containsAny(text, "workflow", "provider", "subprocess", "external", "credential", "auth", "environment") {
		return false
	}
	owned := containsAny(text, "goobers-owned", "goobers owned", "goobers invariant")
	reproduced := containsAny(text, "clean reproduction", "reproduced on supported", "regression comparison")
	return owned && reproduced
}

func canPromoteProductCandidate(bundle diagnostics.Bundle) bool {
	if len(bundle.Notes) > 0 {
		return false
	}
	return versionKnownSupported(bundle.Binary.Version)
}

func versionUnsupported(version string) bool {
	release, ok := releaseLine(version)
	if !ok {
		return false
	}
	current, ok := releaseLine(supportmatrix.NextPlannedRelease)
	if !ok {
		return false
	}
	previous, hasPrevious := previousReleaseLine(current)
	switch {
	case sameReleaseLine(release, current):
		return false
	case hasPrevious && sameReleaseLine(release, previous):
		return false
	case compareReleaseLine(release, current) > 0:
		return false
	default:
		return true
	}
}

func versionKnownSupported(version string) bool {
	release, ok := releaseLine(version)
	if !ok {
		return false
	}
	current, ok := releaseLine(supportmatrix.NextPlannedRelease)
	if !ok {
		return false
	}
	if sameReleaseLine(release, current) {
		return true
	}
	previous, ok := previousReleaseLine(current)
	return ok && sameReleaseLine(release, previous)
}

type releaseLineVersion struct {
	major uint64
	minor uint64
}

func releaseLine(version string) (releaseLineVersion, bool) {
	version = strings.TrimSpace(version)
	if version == "" || version == "dev" || !strings.HasPrefix(version, "v") {
		return releaseLineVersion{}, false
	}
	if before, _, ok := strings.Cut(version, "-"); ok {
		version = before
	}
	parts := strings.Split(strings.TrimPrefix(version, "v"), ".")
	if len(parts) != 3 {
		return releaseLineVersion{}, false
	}
	numbers := make([]uint64, 2)
	for i := 0; i < 2; i++ {
		part := parts[i]
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return releaseLineVersion{}, false
		}
		var value uint64
		for _, r := range part {
			if r < '0' || r > '9' {
				return releaseLineVersion{}, false
			}
			value = value*10 + uint64(r-'0')
		}
		numbers[i] = value
	}
	patch := parts[2]
	if patch == "" || (len(patch) > 1 && patch[0] == '0') {
		return releaseLineVersion{}, false
	}
	for _, r := range patch {
		if r < '0' || r > '9' {
			return releaseLineVersion{}, false
		}
	}
	return releaseLineVersion{major: numbers[0], minor: numbers[1]}, true
}

func previousReleaseLine(current releaseLineVersion) (releaseLineVersion, bool) {
	if current.minor == 0 {
		if current.major == 0 {
			return releaseLineVersion{}, false
		}
		return releaseLineVersion{major: current.major - 1}, true
	}
	return releaseLineVersion{major: current.major, minor: current.minor - 1}, true
}

func sameReleaseLine(left, right releaseLineVersion) bool {
	return left.major == right.major && left.minor == right.minor
}

func compareReleaseLine(left, right releaseLineVersion) int {
	switch {
	case left.major < right.major:
		return -1
	case left.major > right.major:
		return 1
	case left.minor < right.minor:
		return -1
	case left.minor > right.minor:
		return 1
	default:
		return 0
	}
}

func hasContradiction(signals []signal) bool {
	dispositions := uniqueDispositions(signals)
	if len(dispositions) == 0 {
		return false
	}
	for _, disposition := range dispositions {
		if disposition == DispositionInsufficientEvidence && len(dispositions) > 1 {
			return true
		}
	}
	return len(dispositions) > 1
}

func uniqueDispositions(signals []signal) []Disposition {
	seen := map[Disposition]bool{}
	for _, signal := range signals {
		seen[signal.disposition] = true
	}
	out := make([]Disposition, 0, len(seen))
	for disposition := range seen {
		out = append(out, disposition)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func evidenceFromSignals(signals []signal) []Evidence {
	out := make([]Evidence, 0, len(signals))
	for _, signal := range signals {
		out = append(out, signal.evidence)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].Subject != out[j].Subject {
			return out[i].Subject < out[j].Subject
		}
		return out[i].Detail < out[j].Detail
	})
	return out
}

func summarizeCase(bundle diagnostics.Bundle) CaseSummary {
	summary := CaseSummary{
		Binary:   bundle.Binary.Version,
		Platform: strings.Trim(bundle.Binary.OS+"/"+bundle.Binary.Arch, "/"),
	}
	for _, run := range bundle.Runs {
		summary.RunIDs = append(summary.RunIDs, run.RunID)
		summary.Workflows = append(summary.Workflows, run.Workflow)
		summary.Gaggles = append(summary.Gaggles, run.Gaggle)
	}
	summary.RunIDs = dedupeSorted(summary.RunIDs)
	summary.Workflows = dedupeSorted(summary.Workflows)
	summary.Gaggles = dedupeSorted(summary.Gaggles)
	summary.Fingerprint = fingerprint(bundle)
	return summary
}

func summarizeCluster(bundle diagnostics.Bundle) ClusterInfo {
	cluster := ClusterInfo{OccurrenceCount: len(bundle.Runs)}
	if bundle.Binary.Version != "" {
		cluster.AffectedVersions = []string{bundle.Binary.Version}
	}
	platform := strings.Trim(bundle.Binary.OS+"/"+bundle.Binary.Arch, "/")
	if platform != "" {
		cluster.AffectedPlatforms = []string{platform}
	}
	return cluster
}

func fingerprint(bundle diagnostics.Bundle) string {
	var parts []string
	for _, run := range bundle.Runs {
		parts = append(parts, run.Workflow, run.Phase)
		for _, err := range runErrors(run) {
			parts = append(parts, err.Stage, err.Code, normalizeFingerprintText(err.Message))
		}
	}
	if len(parts) == 0 {
		for _, issue := range bundle.Instance.ConfigIssues {
			parts = append(parts, normalizeFingerprintText(issue))
		}
	}
	return strings.Join(dedupeSorted(parts), "|")
}

func normalizeFingerprintText(value string) string {
	value = lowerJoined(value)
	if len(value) > 160 {
		value = value[:160]
	}
	return value
}

func confidenceFor(disposition Disposition) string {
	switch disposition {
	case DispositionGoobersDefectCandidate:
		return "medium"
	case DispositionInsufficientEvidence:
		return "low"
	default:
		return "high"
	}
}

func nextActionFor(disposition Disposition) string {
	switch disposition {
	case DispositionWorkflowConfiguration:
		return "Fix the instance or workflow configuration before filing a Goobers defect."
	case DispositionHarnessAuthentication:
		return "Restore the required harness credential and rerun before filing a Goobers defect."
	case DispositionLocalEnvironment:
		return "Fix the local runner environment and rerun before filing a Goobers defect."
	case DispositionExternalDependency:
		return "Route to the external provider or wait for provider recovery; do not file a Goobers defect yet."
	case DispositionTransientFailure:
		return "Retry or gather repeated failures before filing a Goobers defect."
	case DispositionUnsupportedVersion:
		return "Upgrade to a supported Goobers release and retry."
	case DispositionDuplicateOrKnown:
		return "Link the existing known issue instead of filing a duplicate."
	case DispositionNotReproduced:
		return "Keep as a support case unless a supported reproduction is provided."
	case DispositionGoobersDefectCandidate:
		return "Ask for explicit confirmation before filing a sanitized Goobers issue."
	default:
		return "Request more evidence before filing a Goobers defect."
	}
}

func lowerJoined(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(value)), " ")
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func dedupeSorted(values []string) []string {
	sort.Strings(values)
	out := values[:0]
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
}
