// Package gagglehealth defines the versioned gaggle-health contract: policy
// defaults and validation, structured findings and their episode identity,
// deterministic aggregate-state precedence, and journal replay into per-gaggle
// health snapshots. Detection and repair live in later consumers (#4423).
package gagglehealth

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/secretpattern"
)

// Wire bounds for finding text, evidence, history, and identity fields.
const (
	MaxSummaryLength            = 1024
	MaxEvidenceAssessmentLength = 1024
	MaxActionLength             = 512
	MaxResultSummaryLength      = 1024
	MaxEvidenceDetailLength     = 512
	MaxEvidencePerFinding       = 16
	MaxHistoryFindings          = 1000
	MaxIdentityLength           = 253
	MaxEvidenceKindLength       = 64
)

var findingTextScrubber = secretpattern.NewScrubber()

// Stable finding codes understood by the hard safety policy.
const (
	FindingTriggerSilence       = "trigger-silence"
	FindingNoProgress           = "no-progress"
	FindingWorkflowFlapping     = "workflow-flapping"
	FindingProlongedDegradation = "prolonged-degradation"
	FindingOrphanedClaim        = "orphaned-claim"
	FindingProjectionDrift      = "projection-drift"
	FindingControllerDegraded   = "controller-degraded"
)

type findingRule struct {
	severity   apiv1.GaggleHealthSeverity
	state      apiv1.GaggleHealthState
	safeRepair bool
}

var findingRules = map[string]findingRule{
	FindingTriggerSilence:       {apiv1.GaggleHealthSeverityWarning, apiv1.GaggleHealthDegraded, false},
	FindingNoProgress:           {apiv1.GaggleHealthSeverityError, apiv1.GaggleHealthStalled, false},
	FindingWorkflowFlapping:     {apiv1.GaggleHealthSeverityError, apiv1.GaggleHealthInconsistent, false},
	FindingProlongedDegradation: {apiv1.GaggleHealthSeverityCritical, apiv1.GaggleHealthOperatorRequired, false},
	FindingOrphanedClaim:        {apiv1.GaggleHealthSeverityError, apiv1.GaggleHealthInconsistent, true},
	FindingProjectionDrift:      {apiv1.GaggleHealthSeverityError, apiv1.GaggleHealthInconsistent, false},
	FindingControllerDegraded:   {apiv1.GaggleHealthSeverityError, apiv1.GaggleHealthDegraded, false},
}

// DefaultPolicy preserves existing behavior: observe health, notify operators,
// and authorize only the product's pre-existing idempotent claim repair.
func DefaultPolicy() apiv1.GaggleHealthPolicy {
	enabled := true
	notificationsEnabled := true
	return apiv1.GaggleHealthPolicy{
		Enabled:            &enabled,
		EvaluationInterval: "5m",
		Thresholds: &apiv1.GaggleHealthThresholds{
			TriggerSilence:       "30m",
			NoProgress:           "30m",
			FlappingWindow:       "1h",
			FlappingCount:        3,
			ProlongedDegradation: "6h",
			EvidenceRetention:    "720h",
		},
		Findings: map[string]apiv1.GaggleFindingPolicy{
			FindingOrphanedClaim: {Mode: string(apiv1.GaggleHealthRepairMode)},
		},
		Notifications: &apiv1.GaggleHealthNotifications{
			Enabled:         &notificationsEnabled,
			EscalateAfter:   "1h",
			MinimumSeverity: string(apiv1.GaggleHealthSeverityWarning),
		},
	}
}

// ResolvePolicy applies behavior-safe defaults without mutating configuration.
func ResolvePolicy(config *apiv1.GaggleHealthPolicy) (apiv1.GaggleHealthPolicy, error) {
	resolved := DefaultPolicy()
	if config == nil {
		return resolved, nil
	}
	if config.Enabled != nil {
		value := *config.Enabled
		resolved.Enabled = &value
	}
	if config.EvaluationInterval != "" {
		resolved.EvaluationInterval = config.EvaluationInterval
	}
	if config.Thresholds != nil {
		if config.Thresholds.TriggerSilence != "" {
			resolved.Thresholds.TriggerSilence = config.Thresholds.TriggerSilence
		}
		if config.Thresholds.NoProgress != "" {
			resolved.Thresholds.NoProgress = config.Thresholds.NoProgress
		}
		if config.Thresholds.FlappingWindow != "" {
			resolved.Thresholds.FlappingWindow = config.Thresholds.FlappingWindow
		}
		if config.Thresholds.FlappingCount != 0 {
			resolved.Thresholds.FlappingCount = config.Thresholds.FlappingCount
		}
		if config.Thresholds.ProlongedDegradation != "" {
			resolved.Thresholds.ProlongedDegradation = config.Thresholds.ProlongedDegradation
		}
		if config.Thresholds.EvidenceRetention != "" {
			resolved.Thresholds.EvidenceRetention = config.Thresholds.EvidenceRetention
		}
	}
	for code, policy := range config.Findings {
		resolved.Findings[code] = policy
	}
	if config.Notifications != nil {
		if config.Notifications.Enabled != nil {
			value := *config.Notifications.Enabled
			resolved.Notifications.Enabled = &value
		}
		if config.Notifications.EscalateAfter != "" {
			resolved.Notifications.EscalateAfter = config.Notifications.EscalateAfter
		}
		if config.Notifications.MinimumSeverity != "" {
			resolved.Notifications.MinimumSeverity = config.Notifications.MinimumSeverity
		}
	}
	if config.EventWorkflow != nil {
		workflow := *config.EventWorkflow
		workflow.EventTypes = append([]string(nil), config.EventWorkflow.EventTypes...)
		workflow.FindingCodes = append([]string(nil), config.EventWorkflow.FindingCodes...)
		resolved.EventWorkflow = &workflow
	}
	if err := ValidatePolicy(resolved); err != nil {
		return apiv1.GaggleHealthPolicy{}, err
	}
	return resolved, nil
}

// ValidatePolicy rejects invalid thresholds, unknown finding policies, unsafe
// remediation modes, severity downgrades, and malformed notification or
// event-workflow settings.
func ValidatePolicy(policy apiv1.GaggleHealthPolicy) error {
	if err := validatePolicyThresholds(policy); err != nil {
		return err
	}
	if err := validateFindingPolicies(policy.Findings); err != nil {
		return err
	}
	if err := validateNotificationPolicy(policy.Notifications); err != nil {
		return err
	}
	return validateEventWorkflowPolicy(policy.EventWorkflow)
}

func validatePolicyThresholds(policy apiv1.GaggleHealthPolicy) error {
	interval, err := duration("evaluationInterval", policy.EvaluationInterval, 5*time.Minute)
	if err != nil {
		return err
	}
	if interval < 30*time.Second || interval > time.Hour {
		return fmt.Errorf("evaluationInterval must be between 30s and 1h")
	}
	thresholds := DefaultPolicy().Thresholds
	if policy.Thresholds != nil {
		thresholds = policy.Thresholds
	}
	windows := []struct {
		field    string
		value    string
		fallback time.Duration
	}{
		{"thresholds.triggerSilence", thresholds.TriggerSilence, 30 * time.Minute},
		{"thresholds.noProgress", thresholds.NoProgress, 30 * time.Minute},
		{"thresholds.flappingWindow", thresholds.FlappingWindow, time.Hour},
		{"thresholds.prolongedDegradation", thresholds.ProlongedDegradation, 6 * time.Hour},
	}
	var prolonged time.Duration
	for _, window := range windows {
		value, err := duration(window.field, window.value, window.fallback)
		if err != nil {
			return err
		}
		if value < interval {
			return fmt.Errorf("%s must be at least evaluationInterval", window.field)
		}
		prolonged = value // last window is prolongedDegradation
	}
	retention, err := duration("thresholds.evidenceRetention", thresholds.EvidenceRetention, 30*24*time.Hour)
	if err != nil {
		return err
	}
	count := thresholds.FlappingCount
	if count == 0 {
		count = 3
	}
	if count < 2 || count > 100 {
		return fmt.Errorf("thresholds.flappingCount must be between 2 and 100")
	}
	if retention < prolonged || retention > 90*24*time.Hour {
		return fmt.Errorf("thresholds.evidenceRetention must be at least prolongedDegradation and no more than 2160h")
	}
	return nil
}

func validateFindingPolicies(findings map[string]apiv1.GaggleFindingPolicy) error {
	for code, configured := range findings {
		rule, ok := findingRules[code]
		if !ok {
			return fmt.Errorf("findings.%s is not a known finding policy", code)
		}
		mode := apiv1.GaggleHealthRemediationMode(configured.Mode)
		if mode != apiv1.GaggleHealthObserve && mode != apiv1.GaggleHealthRepairMode && mode != apiv1.GaggleHealthEscalate {
			return fmt.Errorf("findings.%s.mode must be observe, repair, or escalate", code)
		}
		if mode == apiv1.GaggleHealthRepairMode && !rule.safeRepair {
			return fmt.Errorf("findings.%s.mode repair is not authorized by hard safety policy", code)
		}
		if configured.Severity == "" {
			continue
		}
		override := apiv1.GaggleHealthSeverity(configured.Severity)
		if severityRank(override) == 0 {
			return fmt.Errorf("findings.%s.severity is invalid", code)
		}
		if severityRank(override) < severityRank(rule.severity) {
			return fmt.Errorf("findings.%s.severity cannot lower the hard minimum %s", code, rule.severity)
		}
	}
	return nil
}

func validateNotificationPolicy(notifications *apiv1.GaggleHealthNotifications) error {
	if notifications == nil {
		return nil
	}
	if _, err := duration("notifications.escalateAfter", notifications.EscalateAfter, time.Hour); err != nil {
		return err
	}
	if notifications.MinimumSeverity != "" &&
		severityRank(apiv1.GaggleHealthSeverity(notifications.MinimumSeverity)) == 0 {
		return fmt.Errorf("notifications.minimumSeverity is invalid")
	}
	return nil
}

func validateEventWorkflowPolicy(trigger *apiv1.GaggleHealthEventWorkflow) error {
	if trigger == nil {
		return nil
	}
	if trigger.Enabled && strings.TrimSpace(trigger.Workflow) == "" {
		return fmt.Errorf("eventWorkflow.workflow is required when eventWorkflow.enabled is true")
	}
	for _, code := range trigger.FindingCodes {
		if _, ok := findingRules[code]; !ok {
			return fmt.Errorf("eventWorkflow.findingCodes contains unknown finding code %q", code)
		}
	}
	for _, eventType := range trigger.EventTypes {
		if !validEventType(apiv1.GaggleHealthEventType(eventType)) {
			return fmt.Errorf("eventWorkflow.eventTypes contains unknown event type %q", eventType)
		}
	}
	if trigger.MinimumSeverity != "" &&
		severityRank(apiv1.GaggleHealthSeverity(trigger.MinimumSeverity)) == 0 {
		return fmt.Errorf("eventWorkflow.minimumSeverity is invalid")
	}
	return nil
}

func duration(field, value string, fallback time.Duration) (time.Duration, error) {
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive Go duration", field)
	}
	return parsed, nil
}

func severityRank(severity apiv1.GaggleHealthSeverity) int {
	switch severity {
	case apiv1.GaggleHealthSeverityInfo:
		return 1
	case apiv1.GaggleHealthSeverityWarning:
		return 2
	case apiv1.GaggleHealthSeverityError:
		return 3
	case apiv1.GaggleHealthSeverityCritical:
		return 4
	default:
		return 0
	}
}

// EpisodeKey returns a stable, non-secret dedupe identity from the finding's
// code and provider-neutral scope.
func EpisodeKey(code string, identity apiv1.GaggleHealthIdentity) (string, error) {
	if _, ok := findingRules[code]; !ok {
		return "", fmt.Errorf("unknown finding code %q", code)
	}
	if identity.Gaggle == "" {
		return "", errors.New("finding identity gaggle is required")
	}
	data, err := json.Marshal(struct {
		Code     string                     `json:"code"`
		Identity apiv1.GaggleHealthIdentity `json:"identity"`
	}{Code: code, Identity: identity})
	if err != nil {
		return "", fmt.Errorf("encode episode identity: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// ExtendEpisode merges a repeated observation without creating another episode.
func ExtendEpisode(current, observed apiv1.GaggleHealthFinding) (apiv1.GaggleHealthFinding, error) {
	if current.EpisodeKey == "" || current.EpisodeKey != observed.EpisodeKey ||
		current.Code != observed.Code || current.Identity.Gaggle != observed.Identity.Gaggle {
		return apiv1.GaggleHealthFinding{}, errors.New("observation does not match the current episode")
	}
	if observed.LastObserved.Before(current.LastObserved) {
		return apiv1.GaggleHealthFinding{}, errors.New("observation time moves episode backwards")
	}
	observed.FirstObserved = current.FirstObserved
	observed.ObservationCount = current.ObservationCount + 1
	observed.Evidence = append(append([]apiv1.GaggleHealthEvidence{}, current.Evidence...), observed.Evidence...)
	if len(observed.Evidence) > MaxEvidencePerFinding {
		observed.Evidence = observed.Evidence[len(observed.Evidence)-MaxEvidencePerFinding:]
	}
	return observed, ValidateFinding(observed)
}

// ValidateFinding enforces the finding contract: a known code and schema, an
// episode key derived from code and identity, hard severity and repair-safety
// minimums, bounded redacted text and evidence, and a consistent repair record.
func ValidateFinding(finding apiv1.GaggleHealthFinding) error {
	rule, ok := findingRules[finding.Code]
	if !ok {
		return fmt.Errorf("unknown finding code %q", finding.Code)
	}
	if finding.SchemaVersion != apiv1.GaggleHealthSchemaVersion {
		return fmt.Errorf("unsupported finding schemaVersion %q", finding.SchemaVersion)
	}
	if err := validateFindingEpisode(finding); err != nil {
		return err
	}
	if err := validateFindingClassification(finding, rule); err != nil {
		return err
	}
	if err := validateFindingText(finding); err != nil {
		return err
	}
	if err := validateFindingEvidence(finding); err != nil {
		return err
	}
	if err := validateFindingRepair(finding.Repair, rule); err != nil {
		return err
	}
	if finding.ResolvedAt != nil && len(finding.ResolutionEvidence) == 0 {
		return errors.New("resolved finding requires resolution evidence")
	}
	return nil
}

func validateFindingEpisode(finding apiv1.GaggleHealthFinding) error {
	if finding.Identity.Gaggle == "" || finding.EpisodeKey == "" {
		return errors.New("finding gaggle and episodeKey are required")
	}
	expectedKey, err := EpisodeKey(finding.Code, finding.Identity)
	if err != nil {
		return err
	}
	if finding.EpisodeKey != expectedKey || !validDigest(finding.EpisodeKey) {
		return errors.New("finding episodeKey does not match its code and identity")
	}
	if err := validateIdentity(finding.Identity); err != nil {
		return err
	}
	if finding.ObservationCount == 0 || finding.FirstObserved.After(finding.LastObserved) {
		return errors.New("finding observation timestamps or count are invalid")
	}
	return nil
}

func validateFindingClassification(finding apiv1.GaggleHealthFinding, rule findingRule) error {
	if severityRank(finding.Severity) < severityRank(rule.severity) {
		return fmt.Errorf("finding severity is below the hard minimum %s", rule.severity)
	}
	if stateRank(finding.Contribution) == 0 || finding.Contribution == apiv1.GaggleHealthHealthy {
		return errors.New("finding contribution must be an unhealthy or recovering state")
	}
	if finding.Confidence < 0 || finding.Confidence > 1 {
		return errors.New("finding confidence must be between 0 and 1")
	}
	return nil
}

func validateFindingText(finding apiv1.GaggleHealthFinding) error {
	if utf8.RuneCountInString(finding.Summary) > MaxSummaryLength ||
		utf8.RuneCountInString(finding.EvidenceAssessment) > MaxEvidenceAssessmentLength ||
		utf8.RuneCountInString(finding.Repair.RecommendedAction) > MaxActionLength ||
		utf8.RuneCountInString(finding.Repair.ResultSummary) > MaxResultSummaryLength {
		return errors.New("finding contains an unbounded human-readable field")
	}
	if finding.Summary == "" || finding.EvidenceAssessment == "" ||
		finding.Repair.RecommendedAction == "" || len(finding.Evidence) == 0 {
		return errors.New("finding requires a summary, evidence assessment, recommended action, and evidence")
	}
	for _, text := range []string{finding.Summary, finding.EvidenceAssessment, finding.Repair.RecommendedAction, finding.Repair.ResultSummary} {
		if err := validateRedactedText(text); err != nil {
			return err
		}
	}
	return nil
}

func validateFindingEvidence(finding apiv1.GaggleHealthFinding) error {
	if len(finding.Evidence) > MaxEvidencePerFinding || len(finding.ResolutionEvidence) > MaxEvidencePerFinding {
		return errors.New("finding contains too many evidence references")
	}
	for _, evidence := range append(append([]apiv1.GaggleHealthEvidence{}, finding.Evidence...), finding.ResolutionEvidence...) {
		if evidence.Kind == "" || utf8.RuneCountInString(evidence.Kind) > MaxEvidenceKindLength ||
			utf8.RuneCountInString(evidence.Run) > MaxIdentityLength ||
			utf8.RuneCountInString(evidence.Detail) > MaxEvidenceDetailLength {
			return errors.New("finding evidence fields do not satisfy wire bounds")
		}
		if evidence.Digest != "" && !validDigest(evidence.Digest) {
			return errors.New("finding evidence digest must be 64 lowercase hexadecimal characters")
		}
		for _, text := range []string{evidence.Kind, evidence.Run, evidence.Detail} {
			if err := validateRedactedText(text); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateFindingRepair(repair apiv1.GaggleHealthRepair, rule findingRule) error {
	if utf8.RuneCountInString(repair.IdempotencyKey) > MaxIdentityLength {
		return errors.New("finding repair idempotencyKey exceeds the wire bound")
	}
	if err := validateIdentifier("repair idempotencyKey", repair.IdempotencyKey, false); err != nil {
		return err
	}
	if repair.PolicyAuthorized && !rule.safeRepair {
		return errors.New("finding repair authorization violates hard safety policy")
	}
	if err := validateRepairDisposition(repair); err != nil {
		return err
	}
	switch repair.FollowUp {
	case apiv1.GaggleHealthFollowUpNone, apiv1.GaggleHealthFollowUpVerifying,
		apiv1.GaggleHealthFollowUpResolved, apiv1.GaggleHealthFollowUpEscalated:
		return nil
	default:
		return errors.New("finding repair follow-up state is invalid")
	}
}

func validateRepairDisposition(repair apiv1.GaggleHealthRepair) error {
	switch repair.Disposition {
	case apiv1.GaggleHealthRepairNotAttempted:
		if repair.AttemptedAt != nil || repair.CompletedAt != nil || repair.IdempotencyKey != "" {
			return errors.New("not-attempted repair cannot contain attempt state")
		}
	case apiv1.GaggleHealthRepairStarted:
		if repair.AttemptedAt == nil || repair.CompletedAt != nil || repair.IdempotencyKey == "" {
			return errors.New("started repair requires attemptedAt and idempotencyKey but no completedAt")
		}
	case apiv1.GaggleHealthRepairSucceeded, apiv1.GaggleHealthRepairFailed:
		if repair.AttemptedAt == nil || repair.CompletedAt == nil ||
			repair.IdempotencyKey == "" || repair.ResultSummary == "" {
			return errors.New("finished repair requires attempt, completion, idempotency, and result")
		}
	case apiv1.GaggleHealthRepairRefused:
	default:
		return errors.New("finding repair disposition is invalid")
	}
	return nil
}

func validateIdentity(identity apiv1.GaggleHealthIdentity) error {
	values := []struct {
		name     string
		value    string
		required bool
	}{
		{"gaggle", identity.Gaggle, true},
		{"workflow", identity.Workflow, false},
		{"run", identity.Run, false},
		{"stage", identity.Stage, false},
		{"backlogItem", identity.BacklogItem, false},
		{"pullRequest", identity.PullRequest, false},
		{"claim", identity.Claim, false},
		{"runner", identity.Runner, false},
		{"worker", identity.Worker, false},
	}
	for _, value := range values {
		if utf8.RuneCountInString(value.value) > MaxIdentityLength {
			return fmt.Errorf("finding identity %s exceeds the wire bound", value.name)
		}
		if err := validateIdentifier("identity "+value.name, value.value, value.required); err != nil {
			return err
		}
	}
	return nil
}

func validateIdentifier(name, value string, required bool) error {
	if value == "" {
		if required {
			return fmt.Errorf("finding %s is required", name)
		}
		return nil
	}
	if strings.TrimSpace(value) != value || containsControl(value) || rootedPath(value) {
		return fmt.Errorf("finding %s contains whitespace, control data, or a host-private path", name)
	}
	if !bytes.Equal(findingTextScrubber.Scrub([]byte(value)), []byte(value)) {
		return fmt.Errorf("finding %s contains secret-shaped data", name)
	}
	return nil
}

func validateRedactedText(value string) error {
	if value == "" {
		return nil
	}
	if containsControl(value) || containsRootedPath(value) {
		return errors.New("finding text contains control data or a host-private path")
	}
	if !bytes.Equal(findingTextScrubber.Scrub([]byte(value)), []byte(value)) {
		return errors.New("finding text contains secret-shaped data")
	}
	return nil
}

func containsControl(value string) bool {
	for _, r := range value {
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' || r == 0x7f {
			return true
		}
	}
	return false
}

func containsRootedPath(value string) bool {
	for _, field := range strings.Fields(value) {
		candidate := strings.Trim(field, `"'(),.;[]{}<>`)
		if rootedPath(candidate) {
			return true
		}
	}
	return false
}

func rootedPath(value string) bool {
	return filepath.IsAbs(value) ||
		filepath.VolumeName(value) != "" ||
		strings.HasPrefix(value, "/") ||
		strings.HasPrefix(value, `\`) ||
		(len(value) >= 3 && value[1] == ':' &&
			(value[2] == '/' || value[2] == '\\') &&
			((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')))
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

// AggregateState applies deterministic precedence to all active findings.
func AggregateState(findings []apiv1.GaggleHealthFinding) apiv1.GaggleHealthState {
	state := apiv1.GaggleHealthHealthy
	for _, finding := range findings {
		if stateRank(finding.Contribution) > stateRank(state) {
			state = finding.Contribution
		}
	}
	return state
}

func stateRank(state apiv1.GaggleHealthState) int {
	switch state {
	case apiv1.GaggleHealthHealthy:
		return 1
	case apiv1.GaggleHealthRecovering:
		return 2
	case apiv1.GaggleHealthDegraded:
		return 3
	case apiv1.GaggleHealthStalled:
		return 4
	case apiv1.GaggleHealthInconsistent:
		return 5
	case apiv1.GaggleHealthOperatorRequired:
		return 6
	default:
		return 0
	}
}

// Rebuild replays one gaggle's authoritative instance-journal stream.
func Rebuild(gaggle string, events []apiv1.GaggleHealthEvent) (apiv1.GaggleHealthSnapshot, error) {
	return rebuild(gaggle, events, 0, time.Time{})
}

// RebuildWithRetention replays a gaggle and omits resolved episodes older than
// the configured evidence-retention window. Active episodes are never pruned.
func RebuildWithRetention(gaggle string, events []apiv1.GaggleHealthEvent, retention time.Duration, now time.Time) (apiv1.GaggleHealthSnapshot, error) {
	if retention <= 0 || now.IsZero() {
		return apiv1.GaggleHealthSnapshot{}, errors.New("positive retention and current time are required")
	}
	return rebuild(gaggle, events, retention, now)
}

func rebuild(gaggle string, events []apiv1.GaggleHealthEvent, retention time.Duration, now time.Time) (apiv1.GaggleHealthSnapshot, error) {
	snapshot := apiv1.GaggleHealthSnapshot{
		SchemaVersion: apiv1.GaggleHealthSchemaVersion,
		Gaggle:        gaggle,
		State:         apiv1.GaggleHealthHealthy,
		Active:        []apiv1.GaggleHealthFinding{},
		History:       []apiv1.GaggleHealthFinding{},
	}
	active := map[string]apiv1.GaggleHealthFinding{}
	var previous uint64
	for _, event := range events {
		if err := validateReplayEvent(gaggle, previous, event); err != nil {
			return apiv1.GaggleHealthSnapshot{}, err
		}
		previous = event.Sequence
		if err := applyReplayEvent(&snapshot, active, event, retention, now); err != nil {
			return apiv1.GaggleHealthSnapshot{}, err
		}
		snapshot.UpdatedAt = event.OccurredAt
		snapshot.LastSequence = event.Sequence
	}
	for _, finding := range active {
		snapshot.Active = append(snapshot.Active, finding)
	}
	sort.Slice(snapshot.Active, func(i, j int) bool {
		return snapshot.Active[i].EpisodeKey < snapshot.Active[j].EpisodeKey
	})
	snapshot.State = AggregateState(snapshot.Active)
	return snapshot, nil
}

// validateReplayEvent checks one journal event's envelope, ordering, and
// finding snapshot before it is applied to the projection.
func validateReplayEvent(gaggle string, previous uint64, event apiv1.GaggleHealthEvent) error {
	if event.SchemaVersion != apiv1.GaggleHealthSchemaVersion || event.Gaggle != gaggle {
		return errors.New("health event schema or gaggle does not match projection")
	}
	if event.Sequence <= previous {
		return errors.New("health event sequence is not strictly increasing")
	}
	if !validEventType(event.Type) {
		return fmt.Errorf("unknown health event type %q", event.Type)
	}
	if event.Type == apiv1.GaggleHealthEvaluated {
		if event.Finding != nil || event.EpisodeKey != "" {
			return errors.New("health evaluation must not contain a finding or episode key")
		}
		return nil
	}
	if event.Finding == nil || event.EpisodeKey == "" || event.Finding.EpisodeKey != event.EpisodeKey {
		return errors.New("health transition requires a matching finding snapshot")
	}
	if event.Finding.Identity.Gaggle != event.Gaggle {
		return errors.New("health transition finding belongs to another gaggle")
	}
	if err := ValidateFinding(*event.Finding); err != nil {
		return err
	}
	return validateTransition(event)
}

// applyReplayEvent folds one validated transition into the active episode set
// and the retention-bounded resolved history.
func applyReplayEvent(snapshot *apiv1.GaggleHealthSnapshot, active map[string]apiv1.GaggleHealthFinding, event apiv1.GaggleHealthEvent, retention time.Duration, now time.Time) error {
	switch event.Type {
	case apiv1.GaggleHealthFindingOpened:
		if _, exists := active[event.EpisodeKey]; exists {
			return errors.New("finding-opened duplicates an active episode")
		}
		active[event.EpisodeKey] = *event.Finding
	case apiv1.GaggleHealthFindingUpdated, apiv1.GaggleHealthRepairStartedEvent,
		apiv1.GaggleHealthRepairFinishedEvent, apiv1.GaggleHealthEscalated:
		if _, exists := active[event.EpisodeKey]; !exists {
			return errors.New("finding transition references an inactive episode")
		}
		active[event.EpisodeKey] = *event.Finding
	case apiv1.GaggleHealthFindingResolved:
		if _, exists := active[event.EpisodeKey]; !exists {
			return errors.New("finding-resolved references an inactive episode")
		}
		if event.Finding.ResolvedAt == nil {
			return errors.New("finding-resolved requires resolvedAt")
		}
		delete(active, event.EpisodeKey)
		if retention == 0 || !event.Finding.ResolvedAt.Before(now.Add(-retention)) {
			snapshot.History = append(snapshot.History, *event.Finding)
		}
		if len(snapshot.History) > MaxHistoryFindings {
			snapshot.History = snapshot.History[len(snapshot.History)-MaxHistoryFindings:]
		}
	}
	return nil
}

func validateTransition(event apiv1.GaggleHealthEvent) error {
	finding := event.Finding
	switch event.Type {
	case apiv1.GaggleHealthFindingOpened, apiv1.GaggleHealthFindingUpdated:
		if finding.ResolvedAt != nil {
			return errors.New("active finding transition cannot contain resolved state")
		}
	case apiv1.GaggleHealthRepairStartedEvent:
		if finding.ResolvedAt != nil || finding.Repair.Disposition != apiv1.GaggleHealthRepairStarted {
			return errors.New("repair-started transition requires an active started repair")
		}
	case apiv1.GaggleHealthRepairFinishedEvent:
		if finding.ResolvedAt != nil ||
			(finding.Repair.Disposition != apiv1.GaggleHealthRepairSucceeded &&
				finding.Repair.Disposition != apiv1.GaggleHealthRepairFailed &&
				finding.Repair.Disposition != apiv1.GaggleHealthRepairRefused) {
			return errors.New("repair-finished transition requires an active finished repair")
		}
	case apiv1.GaggleHealthEscalated:
		if finding.ResolvedAt != nil || finding.Repair.FollowUp != apiv1.GaggleHealthFollowUpEscalated {
			return errors.New("escalation transition requires an active escalated follow-up")
		}
	case apiv1.GaggleHealthFindingResolved:
		if finding.ResolvedAt == nil || finding.Repair.FollowUp != apiv1.GaggleHealthFollowUpResolved {
			return errors.New("finding-resolved transition requires resolved state")
		}
	}
	return nil
}

func validEventType(eventType apiv1.GaggleHealthEventType) bool {
	switch eventType {
	case apiv1.GaggleHealthEvaluated, apiv1.GaggleHealthFindingOpened,
		apiv1.GaggleHealthFindingUpdated, apiv1.GaggleHealthRepairStartedEvent,
		apiv1.GaggleHealthRepairFinishedEvent, apiv1.GaggleHealthEscalated,
		apiv1.GaggleHealthFindingResolved:
		return true
	default:
		return false
	}
}
