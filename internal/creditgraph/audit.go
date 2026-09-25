package creditgraph

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

const FaultAuditSchemaVersion = "goobers.dev/backprop/fault-audit/v1"

type FaultDomain string

const (
	FaultDomainProductRuntime FaultDomain = "goobers-product-runtime"
	FaultDomainExternal       FaultDomain = "harness-model-provider-environment"
	FaultDomainWorkflow       FaultDomain = "workflow-definition"
	FaultDomainUnknown        FaultDomain = "mixed-or-unknown"
)

type VerificationState string

const (
	VerificationOpen      VerificationState = "open"
	VerificationPending   VerificationState = "verification-pending"
	VerificationRecovered VerificationState = "recovered"
	VerificationRepeated  VerificationState = "repeated"
)

type FaultAuditConfig struct {
	Now               time.Time
	Since             time.Time
	Until             time.Time
	SampleFloor       int
	MaxObservations   int
	MaxFindings       int
	MaxRunsPerFinding int
	MaxEvidence       int
	Cooldown          time.Duration
	PreviousReports   map[string]time.Time
	FixesAppliedAt    map[string]time.Time
}

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

type FaultAuditReport struct {
	Schema              string         `json:"schema"`
	Mode                string         `json:"mode"`
	Since               time.Time      `json:"since"`
	Until               time.Time      `json:"until"`
	ObservationsScanned int            `json:"observationsScanned"`
	ProductFindings     []FaultFinding `json:"productReliabilityFindings,omitempty"`
	ExternalFindings    []FaultFinding `json:"externalFindings,omitempty"`
	WorkflowFindings    []FaultFinding `json:"workflowFindings,omitempty"`
	UnknownFindings     []FaultFinding `json:"mixedOrUnknownFindings,omitempty"`
	Suppressed          int            `json:"suppressed"`
	Truncated           bool           `json:"truncated,omitempty"`
}

type faultSignal struct {
	observation AttributionObservation
	cause       CauseFinding
	signature   string
	path        []string
	evidence    []AttributionEvidenceLink
}

var unstableSignaturePart = regexp.MustCompile(`(?i)(?:[a-f0-9]{16,}|[0-9]+|[a-z]:\\[^\s]+|/[^\s]+)`)

func AuditFaultDomains(observations []AttributionObservation, config FaultAuditConfig) FaultAuditReport {
	config = normalizeAuditConfig(config)
	report := FaultAuditReport{
		Schema: FaultAuditSchemaVersion, Mode: "report-only",
		Since: config.Since, Until: config.Until,
	}
	selected := selectAuditObservations(observations, config)
	report.ObservationsScanned = len(selected)
	groups := map[string][]faultSignal{}
	for _, observation := range selected {
		for _, cause := range observation.Attribution.Causes {
			signal := makeFaultSignal(observation, cause)
			groups[signal.signature] = append(groups[signal.signature], signal)
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
	signature := text
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

func classifyFaultGroup(signature string, signals []faultSignal, all []AttributionObservation, config FaultAuditConfig) FaultFinding {
	finding := FaultFinding{
		ID:        "backprop-" + fmt.Sprintf("%x", sha256.Sum256([]byte(signature)))[:20],
		Signature: signature, Verification: VerificationOpen,
	}
	runSet, workflowSet, versionSet, environmentSet, pathSet := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	domainCounts := map[FaultDomain]int{}
	confidence := 0.0
	missingProvenance, contradictory := false, false
	for _, signal := range signals {
		runSet[signal.observation.RunID] = true
		workflowSet[signal.observation.Workflow] = true
		versionSet[signal.observation.EffectiveVersion] = true
		for _, environment := range signal.observation.Environments {
			environmentSet[environment] = true
		}
		if len(signal.path) > 0 {
			pathSet[strings.Join(signal.path, "\x00")] = true
		}
		finding.Evidence = append(finding.Evidence, signal.evidence...)
		if signal.observation.Status == RecordInsufficientEvidence || signal.observation.EffectiveVersion == "" ||
			signal.observation.Workflow == "" || len(signal.evidence) == 0 {
			missingProvenance = true
		}
		for _, assumption := range signal.cause.Assumptions {
			if strings.Contains(strings.ToLower(assumption), "contradict") {
				contradictory = true
				finding.CounterEvidence = append(finding.CounterEvidence, assumption)
			}
		}
		domainCounts[signalDomain(signal)]++
		confidence += signal.cause.Confidence
	}
	finding.RunIDs = sortedSet(runSet, config.MaxRunsPerFinding)
	finding.Workflows = sortedSet(workflowSet, 0)
	finding.EffectiveVersions = sortedSet(versionSet, 0)
	finding.Environments = sortedSet(environmentSet, 0)
	for _, encoded := range sortedSet(pathSet, 0) {
		finding.NodePaths = append(finding.NodePaths, strings.Split(encoded, "\x00"))
	}
	sortEvidence(finding.Evidence)
	if len(finding.Evidence) > config.MaxEvidence {
		finding.Evidence = finding.Evidence[:config.MaxEvidence]
	}

	finding.Domain = dominantDomain(domainCounts)
	sparse := len(runSet) < config.SampleFloor
	if sparse || missingProvenance || contradictory || len(domainCounts) != 1 {
		finding.Domain = FaultDomainUnknown
	}
	if finding.Domain == FaultDomainWorkflow &&
		(len(workflowSet) != 1 || len(versionSet) != 1 || len(pathSet) != 1) {
		finding.Domain = FaultDomainUnknown
		finding.CounterEvidence = append(finding.CounterEvidence, "the signature is not localized to one workflow, EffectiveVersion, and node path")
	}
	if (finding.Domain == FaultDomainProductRuntime || finding.Domain == FaultDomainExternal) && len(workflowSet) < 2 {
		finding.Domain = FaultDomainUnknown
		finding.CounterEvidence = append(finding.CounterEvidence, "the signature has not crossed unrelated workflow boundaries")
	}
	finding.Confidence = round(confidence / float64(len(signals)))
	if sparse {
		finding.Confidence = min(finding.Confidence, 0.35)
		finding.CounterEvidence = append(finding.CounterEvidence, fmt.Sprintf("sample floor not met: %d runs observed, %d required", len(runSet), config.SampleFloor))
	}
	if missingProvenance {
		finding.Confidence = min(finding.Confidence, 0.3)
		finding.CounterEvidence = append(finding.CounterEvidence, "one or more observations lack exact version or journal/artifact provenance")
	}
	if contradictory || len(domainCounts) != 1 {
		finding.Confidence = min(finding.Confidence, 0.45)
	}
	if finding.Domain == FaultDomainUnknown {
		finding.Confidence = min(finding.Confidence, 0.45)
	}
	finding.Rationale, finding.AlternativeDomains, finding.RecommendedOwner, finding.RecommendedAction = explainFaultFinding(finding, len(signals))
	finding.Verification = verificationState(finding, signals, all, config)
	return finding
}

func signalDomain(signal faultSignal) FaultDomain {
	text := strings.ToLower(signal.signature + " " + strings.Join(signal.cause.Evidence, " "))
	switch {
	case containsAny(text, "scheduler", "daemon", "worktree", "journal", "claim", "admission", "publication", "shared ci", "recovery"):
		return FaultDomainProductRuntime
	case containsAny(text, "credential", "rate limit", "provider", "model", "network", "filesystem", "antivirus", "operating system", "harness"):
		return FaultDomainExternal
	}
	switch signal.cause.Class {
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
	state := VerificationPending
	for _, observation := range all {
		if observation.ObservedAt.IsZero() || !observation.ObservedAt.After(fixedAt) || !contains(finding.Workflows, observation.Workflow) {
			continue
		}
		state = VerificationRecovered
		for _, signal := range signals {
			if signal.observation.RunID == observation.RunID {
				return VerificationRepeated
			}
		}
	}
	return state
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

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
