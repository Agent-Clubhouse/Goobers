package creditgraph

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// FaultAuditSchemaVersion identifies the persisted fault-audit report contract.
const FaultAuditSchemaVersion = "goobers.dev/backprop/fault-audit/v1"

// FaultDomain classifies the ownership boundary most likely responsible for a failure.
type FaultDomain string

const (
	// FaultDomainProductRuntime identifies failures attributable to Goobers runtime behavior.
	FaultDomainProductRuntime FaultDomain = "goobers-product-runtime"
	// FaultDomainExternal identifies failures attributable to the harness, model, provider, or environment.
	FaultDomainExternal FaultDomain = "harness-model-provider-environment"
	// FaultDomainWorkflow identifies failures attributable to workflow definitions or instructions.
	FaultDomainWorkflow FaultDomain = "workflow-definition"
	// FaultDomainUnknown identifies failures without sufficient evidence for a narrower domain.
	FaultDomainUnknown FaultDomain = "mixed-or-unknown"
)

// VerificationState describes post-fix evidence for a fault finding.
type VerificationState string

const (
	// VerificationOpen identifies a finding without a recorded fix.
	VerificationOpen VerificationState = "open"
	// VerificationPending identifies a fixed finding awaiting a matching held-out observation.
	VerificationPending VerificationState = "verification-pending"
	// VerificationRecovered identifies a fixed finding with a matching healthy observation.
	VerificationRecovered VerificationState = "recovered"
	// VerificationRepeated identifies a fixed finding that recurred in a matching observation.
	VerificationRepeated VerificationState = "repeated"
)

// FaultAuditConfig bounds an audit and supplies durable cooldown and fix state.
type FaultAuditConfig struct {
	Now                  time.Time
	Since                time.Time
	Until                time.Time
	SampleFloor          int
	MaxObservations      int
	MaxFindings          int
	MaxRunsPerFinding    int
	MaxEvidence          int
	Cooldown             time.Duration
	PreviousReports      map[string]time.Time
	FixesAppliedAt       map[string]time.Time
	BaselineObservations map[string][]AttributionObservation
}

// FaultFinding describes one evidence-backed failure signature and its likely owner.
type FaultFinding struct {
	ID                 string                    `json:"id"`
	Signature          string                    `json:"signature"`
	Domain             FaultDomain               `json:"domain"`
	Confidence         float64                   `json:"confidence"`
	RunIDs             []string                  `json:"runIds"`
	Workflows          []string                  `json:"workflows"`
	EffectiveVersions  []string                  `json:"effectiveVersions"`
	Environments       []string                  `json:"environments,omitempty"`
	NodePaths          [][]string                `json:"nodePaths,omitempty"`
	Evidence           []AttributionEvidenceLink `json:"evidence,omitempty"`
	CounterEvidence    []string                  `json:"counterEvidence,omitempty"`
	Rationale          string                    `json:"rationale"`
	AlternativeDomains []string                  `json:"alternativeDomains"`
	RecommendedOwner   string                    `json:"recommendedOwner"`
	RecommendedAction  string                    `json:"recommendedAction"`
	Verification       VerificationState         `json:"verification"`
}

// FaultAuditReport groups findings by ownership boundary.
type FaultAuditReport struct {
	Schema               string                              `json:"schema"`
	Mode                 string                              `json:"mode"`
	Since                time.Time                           `json:"since"`
	Until                time.Time                           `json:"until"`
	ObservationsScanned  int                                 `json:"observationsScanned"`
	ProductFindings      []FaultFinding                      `json:"productReliabilityFindings,omitempty"`
	ExternalFindings     []FaultFinding                      `json:"externalFindings,omitempty"`
	WorkflowFindings     []FaultFinding                      `json:"workflowFindings,omitempty"`
	UnknownFindings      []FaultFinding                      `json:"mixedOrUnknownFindings,omitempty"`
	Suppressed           int                                 `json:"suppressed"`
	Truncated            bool                                `json:"truncated,omitempty"`
	BaselineObservations map[string][]AttributionObservation `json:"-"`
}

type faultSignal struct {
	observation AttributionObservation
	cause       CauseFinding
	signature   string
	path        []string
	evidence    []AttributionEvidenceLink
}

var unstableSignaturePart = regexp.MustCompile(`(?i)(?:[a-f0-9]{16,}|[0-9]+|[a-z]:\\[^\s]+|/[^\s]+)`)

// AuditFaultDomains classifies stored attribution observations into actionable fault domains.
func AuditFaultDomains(observations []AttributionObservation, config FaultAuditConfig) FaultAuditReport {
	config = normalizeAuditConfig(config)
	report := FaultAuditReport{
		Schema: FaultAuditSchemaVersion, Mode: "report-only",
		Since: config.Since, Until: config.Until,
	}
	selected := selectAuditObservations(observations, config)
	report.ObservationsScanned = len(selected)
	groups := map[string][]faultSignal{}
	selectedRuns := map[string]bool{}
	for _, observation := range selected {
		selectedRuns[observation.RunID] = true
		for _, cause := range observation.Attribution.Causes {
			signal := makeFaultSignal(observation, cause)
			groups[signal.signature] = append(groups[signal.signature], signal)
		}
	}
	for id, baseline := range config.BaselineObservations {
		if _, fixed := config.FixesAppliedAt[id]; !fixed {
			continue
		}
		for _, observation := range baseline {
			if selectedRuns[observation.RunID] {
				continue
			}
			for _, cause := range observation.Attribution.Causes {
				signal := makeFaultSignal(observation, cause)
				if faultFindingID(signal.signature) == id {
					groups[signal.signature] = append(groups[signal.signature], signal)
				}
			}
		}
	}
	signatures := make([]string, 0, len(groups))
	for signature := range groups {
		signatures = append(signatures, signature)
	}
	sort.Strings(signatures)
	for _, signature := range signatures {
		finding := classifyFaultGroup(signature, groups[signature], selected, config)
		if reportedAt, ok := config.PreviousReports[finding.ID]; ok &&
			finding.Verification == VerificationOpen &&
			config.Now.Sub(reportedAt) < config.Cooldown {
			report.Suppressed++
			continue
		}
		if len(report.ProductFindings)+len(report.ExternalFindings)+len(report.WorkflowFindings)+len(report.UnknownFindings) >= config.MaxFindings {
			report.Truncated = true
			continue
		}
		if report.BaselineObservations == nil {
			report.BaselineObservations = map[string][]AttributionObservation{}
		}
		report.BaselineObservations[finding.ID] = baselineObservations(
			groups[signature], config.FixesAppliedAt[finding.ID], config.MaxRunsPerFinding,
		)
		switch finding.Domain {
		case FaultDomainProductRuntime:
			report.ProductFindings = append(report.ProductFindings, finding)
		case FaultDomainExternal:
			report.ExternalFindings = append(report.ExternalFindings, finding)
		case FaultDomainWorkflow:
			report.WorkflowFindings = append(report.WorkflowFindings, finding)
		default:
			report.UnknownFindings = append(report.UnknownFindings, finding)
		}
	}
	return report
}

func normalizeAuditConfig(config FaultAuditConfig) FaultAuditConfig {
	if config.Now.IsZero() {
		config.Now = time.Now().UTC()
	}
	if config.Until.IsZero() {
		config.Until = config.Now
	}
	if config.Since.IsZero() {
		config.Since = config.Until.Add(-7 * 24 * time.Hour)
	}
	if config.SampleFloor < 1 {
		config.SampleFloor = 3
	}
	if config.MaxObservations < 1 {
		config.MaxObservations = 500
	}
	if config.MaxFindings < 1 {
		config.MaxFindings = 20
	}
	if config.MaxRunsPerFinding < 1 {
		config.MaxRunsPerFinding = 50
	}
	if config.MaxEvidence < 1 {
		config.MaxEvidence = 20
	}
	if config.Cooldown <= 0 {
		config.Cooldown = 24 * time.Hour
	}
	return config
}

func selectAuditObservations(observations []AttributionObservation, config FaultAuditConfig) []AttributionObservation {
	seen := map[string]bool{}
	selected := make([]AttributionObservation, 0, min(len(observations), config.MaxObservations))
	for _, observation := range observations {
		if observation.RunID == "" || seen[observation.RunID] {
			continue
		}
		if !observation.ObservedAt.IsZero() &&
			(observation.ObservedAt.Before(config.Since) || observation.ObservedAt.After(config.Until)) {
			continue
		}
		seen[observation.RunID] = true
		selected = append(selected, observation)
		if len(selected) == config.MaxObservations {
			break
		}
	}
	return selected
}

func makeFaultSignal(observation AttributionObservation, cause CauseFinding) faultSignal {
	signature := causeSignature(cause)
	var path []string
	if contribution, ok := observation.Attribution.Contribution(cause.NodeID); ok {
		path = contributionPath(contribution)
	}
	var evidence []AttributionEvidenceLink
	for _, link := range observation.Evidence {
		if link.Source == string(cause.Class) && link.NodeID == cause.NodeID &&
			(cause.Stage == "" || link.Stage == cause.Stage) {
			evidence = append(evidence, link)
		}
	}
	return faultSignal{observation: observation, cause: cause, signature: signature, path: path, evidence: evidence}
}

// causeSignature is the stable, run-independent text a cause groups by.
func causeSignature(cause CauseFinding) string {
	text := strings.TrimSpace(cause.Summary)
	if text == "" && len(cause.Evidence) > 0 {
		text = cause.Evidence[0]
	}
	text = strings.ToLower(unstableSignaturePart.ReplaceAllString(text, "#"))
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		text = "stage:" + cause.Stage
	}
	if text == "stage:" {
		text = "unknown-failure"
	}
	return text
}

func faultFindingID(signature string) string {
	return "backprop-" + fmt.Sprintf("%x", sha256.Sum256([]byte(signature)))[:20]
}

func classifyFaultGroup(signature string, signals []faultSignal, all []AttributionObservation, config FaultAuditConfig) FaultFinding {
	finding := FaultFinding{
		ID:        faultFindingID(signature),
		Signature: signature, Verification: VerificationOpen,
	}
	classificationSignals := faultClassificationSignals(finding.ID, signals, config.FixesAppliedAt)
	summary := summarizeFaultSignals(classificationSignals)
	finding.Evidence = summary.evidence
	finding.CounterEvidence = summary.counterEvidence
	finding.RunIDs = sortedSet(summary.runs, config.MaxRunsPerFinding)
	finding.Workflows = sortedSet(summary.workflows, 0)
	finding.EffectiveVersions = sortedSet(summary.versions, 0)
	finding.Environments = sortedSet(summary.environments, 0)
	for _, encoded := range sortedSet(summary.paths, 0) {
		finding.NodePaths = append(finding.NodePaths, strings.Split(encoded, "\x00"))
	}
	sortEvidence(finding.Evidence)
	if len(finding.Evidence) > config.MaxEvidence {
		finding.Evidence = finding.Evidence[:config.MaxEvidence]
	}

	finding.Domain = dominantDomain(summary.domainCounts)
	sparse := len(summary.runs) < config.SampleFloor
	if sparse || summary.missingProvenance || summary.contradictory || len(summary.domainCounts) != 1 {
		finding.Domain = FaultDomainUnknown
	}
	if finding.Domain == FaultDomainWorkflow &&
		(len(summary.workflows) != 1 || len(summary.versions) != 1 || len(summary.paths) != 1) {
		finding.Domain = FaultDomainUnknown
		finding.CounterEvidence = append(finding.CounterEvidence, "the signature is not localized to one workflow, EffectiveVersion, and node path")
	}
	if (finding.Domain == FaultDomainProductRuntime || finding.Domain == FaultDomainExternal) && len(summary.workflows) < 2 {
		finding.Domain = FaultDomainUnknown
		finding.CounterEvidence = append(finding.CounterEvidence, "the signature has not crossed unrelated workflow boundaries")
	}
	finding.Confidence = round(summary.confidence / summary.weight)
	if sparse {
		finding.Confidence = min(finding.Confidence, 0.35)
		finding.CounterEvidence = append(finding.CounterEvidence, fmt.Sprintf("sample floor not met: %d runs observed, %d required", len(summary.runs), config.SampleFloor))
	}
	if summary.missingProvenance {
		finding.Confidence = min(finding.Confidence, 0.3)
		finding.CounterEvidence = append(finding.CounterEvidence, "one or more observations lack exact version or journal/artifact provenance")
	}
	if summary.contradictory || len(summary.domainCounts) != 1 {
		finding.Confidence = min(finding.Confidence, 0.45)
	}
	if finding.Domain == FaultDomainUnknown {
		finding.Confidence = min(finding.Confidence, 0.45)
	}
	finding.Rationale, finding.AlternativeDomains, finding.RecommendedOwner, finding.RecommendedAction = explainFaultFinding(finding, len(classificationSignals))
	finding.Verification = verificationState(finding, signals, all, config)
	return finding
}

func faultClassificationSignals(findingID string, signals []faultSignal, fixesAppliedAt map[string]time.Time) []faultSignal {
	fixedAt, ok := fixesAppliedAt[findingID]
	if !ok {
		return signals
	}
	var baseline []faultSignal
	for _, signal := range signals {
		if signal.observation.ObservedAt.IsZero() || !signal.observation.ObservedAt.After(fixedAt) {
			baseline = append(baseline, signal)
		}
	}
	if len(baseline) == 0 {
		return signals
	}
	return baseline
}

type faultSignalSummary struct {
	runs, workflows, versions, environments, paths map[string]bool
	domainCounts                                   map[FaultDomain]int
	confidence, weight                             float64
	missingProvenance, contradictory               bool
	evidence                                       []AttributionEvidenceLink
	counterEvidence                                []string
}

func summarizeFaultSignals(signals []faultSignal) faultSignalSummary {
	summary := faultSignalSummary{
		runs: map[string]bool{}, workflows: map[string]bool{}, versions: map[string]bool{},
		environments: map[string]bool{}, paths: map[string]bool{}, domainCounts: map[FaultDomain]int{},
	}
	for _, signal := range signals {
		summary.runs[signal.observation.RunID] = true
		summary.workflows[signal.observation.Workflow] = true
		summary.versions[signal.observation.EffectiveVersion] = true
		for _, environment := range signal.observation.Environments {
			summary.environments[environment] = true
		}
		if len(signal.path) > 0 {
			summary.paths[strings.Join(signal.path, "\x00")] = true
		}
		summary.evidence = append(summary.evidence, signal.evidence...)
		if signal.observation.Status == RecordFailed || signal.observation.Status == RecordInsufficientEvidence ||
			signal.observation.EffectiveVersion == "" ||
			signal.observation.Workflow == "" || len(signal.evidence) == 0 {
			summary.missingProvenance = true
		}
		for _, assumption := range signal.cause.Assumptions {
			if strings.Contains(strings.ToLower(assumption), "contradict") {
				summary.contradictory = true
				summary.counterEvidence = append(summary.counterEvidence, assumption)
			}
		}
		summary.domainCounts[signalDomain(signal)]++
		weight := groundTruthWeight(signal.observation.GroundTruth)
		summary.weight += weight
		summary.confidence += weight * groundTruthConfidence(signal.cause.Confidence, signal.observation.GroundTruth)
	}
	return summary
}

func baselineObservations(signals []faultSignal, fixedAt time.Time, limit int) []AttributionObservation {
	seen := map[string]bool{}
	baseline := make([]AttributionObservation, 0, len(signals))
	for _, signal := range signals {
		observation := signal.observation
		if !fixedAt.IsZero() && observation.ObservedAt.After(fixedAt) {
			continue
		}
		if seen[observation.RunID] {
			continue
		}
		seen[observation.RunID] = true
		baseline = append(baseline, observation)
		if len(baseline) >= limit {
			break
		}
	}
	return baseline
}

func signalDomain(signal faultSignal) FaultDomain {
	return CauseFaultDomain(signal.cause)
}

// CauseFaultDomain is the fault domain the auditor assigns one cause before
// cohort rules (sparse samples, mixed domains, boundary crossing) apply: a
// cause whose text names a shared runtime or external component routes there,
// and otherwise its class decides.
func CauseFaultDomain(cause CauseFinding) FaultDomain {
	text := strings.ToLower(causeSignature(cause) + " " + strings.Join(cause.Evidence, " "))
	switch {
	case containsAny(text, "scheduler", "daemon", "worktree", "journal", "claim", "admission", "publication", "shared ci", "recovery"):
		return FaultDomainProductRuntime
	case containsAny(text, "credential", "rate limit", "provider", "model", "network", "filesystem", "antivirus", "operating system", "harness"):
		return FaultDomainExternal
	}
	return ClassFaultDomain(cause.Class)
}

// ClassFaultDomain is the fault domain a failure class implies when the
// cause's text names no shared runtime or external component.
func ClassFaultDomain(class FailureClass) FaultDomain {
	switch class {
	case ClassBadToolChoice, ClassBadInterpretation, ClassWeakInstructions, ClassRouting, ClassTopology:
		return FaultDomainWorkflow
	case ClassModel, ClassEnvironment:
		return FaultDomainExternal
	default:
		return FaultDomainUnknown
	}
}

func dominantDomain(counts map[FaultDomain]int) FaultDomain {
	for domain := range counts {
		return domain
	}
	return FaultDomainUnknown
}

func explainFaultFinding(finding FaultFinding, samples int) (string, []string, string, string) {
	scope := fmt.Sprintf("%d unique runs across %d workflows and %d EffectiveVersions", len(finding.RunIDs), len(finding.Workflows), len(finding.EffectiveVersions))
	switch finding.Domain {
	case FaultDomainProductRuntime:
		return scope + " share a product/runtime signature; workflow ownership was rejected because the failure crosses workflow boundaries.",
			[]string{"workflow-definition rejected: signature spans unrelated workflows", "external retained only if later evidence identifies a harness, provider, or environment component"},
			"Goobers product reliability", "investigate the shared runtime component using the linked journals and artifacts"
	case FaultDomainExternal:
		return scope + " share a harness/model/provider/environment signature; workflow ownership was rejected because the failure crosses workflow boundaries.",
			[]string{"workflow-definition rejected: signature spans unrelated workflows", "product/runtime retained if shared Goobers infrastructure is later identified"},
			"harness/provider/environment owner", "investigate the shared external component and preserve provider/environment evidence"
	case FaultDomainWorkflow:
		return scope + " localize to one workflow, EffectiveVersion, and node path.",
			[]string{"product/runtime rejected: no cross-workflow occurrence", "external rejected: evidence identifies a workflow-controlled stage, topology, instruction, or tool choice"},
			"workflow/config owner", "remediate the pinned workflow version and verify on a held-out post-fix cohort"
	default:
		return fmt.Sprintf("%s produced %d signals, but sparse, contradictory, mixed, or incomplete evidence prevents a single-owner classification.", scope, samples),
			[]string{"product/runtime retained pending cross-workflow evidence", "external retained pending component provenance", "workflow-definition retained pending localization"},
			"Backprop triage", "collect more exact provenance and do not file an owner-specific issue yet"
	}
}

func verificationState(finding FaultFinding, signals []faultSignal, all []AttributionObservation, config FaultAuditConfig) VerificationState {
	fixedAt, ok := config.FixesAppliedAt[finding.ID]
	if !ok {
		return VerificationOpen
	}
	var baseline []faultSignal
	repeatedRuns := map[string]bool{}
	for _, signal := range signals {
		if signal.observation.ObservedAt.IsZero() || !signal.observation.ObservedAt.After(fixedAt) {
			baseline = append(baseline, signal)
			continue
		}
		repeatedRuns[signal.observation.RunID] = true
	}
	affectedWorkflows := map[string]bool{}
	for _, signal := range baseline {
		if signal.observation.Workflow != "" {
			affectedWorkflows[signal.observation.Workflow] = true
		}
	}
	recoveredRuns := map[string]bool{}
	recoveredWorkflows := map[string]bool{}
	for _, observation := range all {
		repeated := repeatedRuns[observation.RunID]
		if observation.ObservedAt.IsZero() || !observation.ObservedAt.After(fixedAt) ||
			!matchesVerificationCohort(observation, baseline, finding.Domain, repeated) {
			continue
		}
		if repeated {
			return VerificationRepeated
		}
		if observation.Status != RecordComplete ||
			observation.RunPhase != journal.PhaseCompleted ||
			len(observation.Attribution.Causes) > 0 {
			continue
		}
		recoveredRuns[observation.RunID] = true
		recoveredWorkflows[observation.Workflow] = true
	}
	if len(recoveredRuns) < config.SampleFloor {
		return VerificationPending
	}
	for workflow := range affectedWorkflows {
		if !recoveredWorkflows[workflow] {
			return VerificationPending
		}
	}
	return VerificationRecovered
}

func matchesVerificationCohort(
	observation AttributionObservation,
	baseline []faultSignal,
	domain FaultDomain,
	repeated bool,
) bool {
	for _, signal := range baseline {
		affected := signal.observation
		if observation.Workflow != affected.Workflow ||
			affected.Workload == "" || observation.Workload != affected.Workload ||
			!sameStringSet(observation.Environments, affected.Environments) ||
			!observationExercisesPath(observation, signal.path) {
			continue
		}
		if verificationVersionMatches(observation, affected, domain, repeated) {
			return true
		}
	}
	return false
}

func verificationVersionMatches(observation, affected AttributionObservation, domain FaultDomain, repeated bool) bool {
	if observation.EffectiveVersion == "" || affected.EffectiveVersion == "" {
		return false
	}
	if repeated && observation.EffectiveVersion == affected.EffectiveVersion {
		return true
	}
	switch domain {
	case FaultDomainWorkflow:
		if observation.WorkflowDigest != "" && affected.WorkflowDigest != "" {
			return observation.WorkflowDigest != affected.WorkflowDigest &&
				sameOptionalValue(observation.GooberDigest, affected.GooberDigest)
		}
		return observation.EffectiveVersion != affected.EffectiveVersion
	case FaultDomainProductRuntime:
		if observation.GooberDigest != "" && affected.GooberDigest != "" {
			return observation.GooberDigest != affected.GooberDigest &&
				sameOptionalValue(observation.WorkflowDigest, affected.WorkflowDigest)
		}
		return observation.EffectiveVersion != affected.EffectiveVersion
	case FaultDomainExternal:
		return sameOptionalValue(observation.WorkflowDigest, affected.WorkflowDigest) &&
			sameOptionalValue(observation.GooberDigest, affected.GooberDigest)
	default:
		return observation.EffectiveVersion == affected.EffectiveVersion
	}
}

func sameOptionalValue(left, right string) bool {
	return left == "" || right == "" || left == right
}

func observationExercisesPath(observation AttributionObservation, affectedPath []string) bool {
	if len(affectedPath) == 0 {
		return false
	}
	for _, contribution := range observation.Attribution.Contributions {
		if slicesEqual(contributionPath(contribution), affectedPath) {
			return true
		}
	}
	return false
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	counts := make(map[string]int, len(left))
	for _, value := range left {
		counts[value]++
	}
	for _, value := range right {
		if counts[value] == 0 {
			return false
		}
		counts[value]--
	}
	return true
}

func slicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func sortedSet(values map[string]bool, limit int) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		if value != "" {
			result = append(result, value)
		}
	}
	sort.Strings(result)
	if limit > 0 && len(result) > limit {
		return result[:limit]
	}
	return result
}

func sortEvidence(evidence []AttributionEvidenceLink) {
	sort.Slice(evidence, func(i, j int) bool {
		if evidence[i].RunID == evidence[j].RunID {
			return evidence[i].JournalSequence < evidence[j].JournalSequence
		}
		return evidence[i].RunID < evidence[j].RunID
	})
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}
