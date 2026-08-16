// Package missioncontrol defines provider-neutral, deterministic launch verdict artifacts.
package missioncontrol

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SchemaVersion identifies the mission-control artifact contract.
const SchemaVersion = "goobers.dev/mission-control-verdict/v1alpha1"

// Verdict is the authoritative launch status of a metric or aggregate.
type Verdict string

// Mission-control verdicts are exhaustive and fail closed to VerdictUnknown.
const (
	VerdictGo      Verdict = "go"
	VerdictNoGo    Verdict = "no-go"
	VerdictUnknown Verdict = "unknown"
)

// ReasonCode explains why a verdict was produced.
type ReasonCode string

// Reason codes cover metric evaluation and aggregate evaluation outcomes.
const (
	ReasonSatisfied            ReasonCode = "satisfied"
	ReasonThresholdViolated    ReasonCode = "threshold-violated"
	ReasonMissing              ReasonCode = "missing"
	ReasonStale                ReasonCode = "stale"
	ReasonQueryError           ReasonCode = "query-error"
	ReasonSchemaError          ReasonCode = "schema-error"
	ReasonUnitError            ReasonCode = "unit-error"
	ReasonInsufficientEvidence ReasonCode = "insufficient-evidence"
	ReasonRequiredNoGo         ReasonCode = "required-no-go"
	ReasonRequiredUnknown      ReasonCode = "required-unknown"
	ReasonAllRequiredGo        ReasonCode = "all-required-go"
	ReasonUnknownAllowed       ReasonCode = "unknown-allowed"
)

// Requirement controls whether an item affects its parent aggregate.
type Requirement string

// Requirements distinguish launch-blocking items from advisory items.
const (
	Required Requirement = "required"
	Advisory Requirement = "advisory"
)

// UnknownPolicy controls whether unknown required items prevent a go verdict.
type UnknownPolicy string

// Unknown policies must explicitly choose fail-closed or permissive aggregation.
const (
	UnknownBlocksGo UnknownPolicy = "block"
	UnknownAllowsGo UnknownPolicy = "allow"
)

// Comparator defines how an observed value is tested against a criterion.
type Comparator string

// Supported comparators include strict and inclusive threshold and range checks.
const (
	GreaterThan    Comparator = "gt"
	GreaterOrEqual Comparator = "gte"
	LessThan       Comparator = "lt"
	LessOrEqual    Comparator = "lte"
	InclusiveRange Comparator = "range-inclusive"
)

// ValueType identifies the numeric representation of an observed value.
type ValueType string

// Supported value types distinguish arbitrary numbers from integers.
const (
	ValueNumber  ValueType = "number"
	ValueInteger ValueType = "integer"
)

// EvidenceRef points to bounded raw query evidence without embedding it.
type EvidenceRef struct {
	ID     string `json:"id"`
	URI    string `json:"uri"`
	Digest string `json:"digest"`
}

// Value is an observed numeric value expressed in a canonical unit.
type Value struct {
	Type   ValueType `json:"type"`
	Number float64   `json:"number"`
	Unit   string    `json:"unit"`
}

// Criterion describes the comparison and canonical unit for a metric.
type Criterion struct {
	Comparator Comparator `json:"comparator"`
	Unit       string     `json:"unit"`
	Threshold  *float64   `json:"threshold,omitempty"`
	Minimum    *float64   `json:"minimum,omitempty"`
	Maximum    *float64   `json:"maximum,omitempty"`
}

// ObservationWindow records the interval covered by an observation.
type ObservationWindow struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// MetricVerdict records one evaluated metric and its complete evidence metadata.
type MetricVerdict struct {
	ID                string            `json:"id"`
	DisplayName       string            `json:"displayName"`
	SubsystemID       string            `json:"subsystemId"`
	Requirement       Requirement       `json:"requirement"`
	EvidenceID        string            `json:"evidenceId,omitempty"`
	Value             *Value            `json:"value,omitempty"`
	Criterion         Criterion         `json:"criterion"`
	DisplayPrecision  int               `json:"displayPrecision"`
	ObservationWindow ObservationWindow `json:"observationWindow"`
	DataAsOf          *time.Time        `json:"dataAsOf,omitempty"`
	RequiredFreshness string            `json:"requiredFreshness"`
	Age               string            `json:"age,omitempty"`
	Verdict           Verdict           `json:"verdict"`
	ReasonCode        ReasonCode        `json:"reasonCode"`
}

// AggregationPolicy defines how unknown required children affect an aggregate.
type AggregationPolicy struct {
	Unknown UnknownPolicy `json:"unknown"`
}

// SubsystemVerdict aggregates required and advisory metrics for one subsystem.
type SubsystemVerdict struct {
	ID                string            `json:"id"`
	DisplayName       string            `json:"displayName"`
	Requirement       Requirement       `json:"requirement"`
	Policy            AggregationPolicy `json:"policy"`
	RequiredMetricIDs []string          `json:"requiredMetricIds"`
	AdvisoryMetricIDs []string          `json:"advisoryMetricIds"`
	Verdict           Verdict           `json:"verdict"`
	ReasonCode        ReasonCode        `json:"reasonCode"`
}

// OverallVerdict aggregates required and advisory subsystem verdicts.
type OverallVerdict struct {
	Policy               AggregationPolicy `json:"policy"`
	RequiredSubsystemIDs []string          `json:"requiredSubsystemIds"`
	AdvisorySubsystemIDs []string          `json:"advisorySubsystemIds"`
	Verdict              Verdict           `json:"verdict"`
	ReasonCode           ReasonCode        `json:"reasonCode"`
}

// Artifact is the versioned provider-neutral mission-control verdict document.
type Artifact struct {
	SchemaVersion string             `json:"schemaVersion"`
	GeneratedAt   time.Time          `json:"generatedAt"`
	Evidence      []EvidenceRef      `json:"evidence"`
	Metrics       []MetricVerdict    `json:"metrics"`
	Subsystems    []SubsystemVerdict `json:"subsystems"`
	Overall       OverallVerdict     `json:"overall"`
}

// MetricDefinition configures deterministic evaluation of one metric.
type MetricDefinition struct {
	ID                string
	DisplayName       string
	SubsystemID       string
	Requirement       Requirement
	Criterion         Criterion
	DisplayPrecision  int
	ObservationWindow ObservationWindow
	RequiredFreshness time.Duration
}

// SubsystemDefinition configures aggregation for one subsystem.
type SubsystemDefinition struct {
	ID          string
	DisplayName string
	Requirement Requirement
	Policy      AggregationPolicy
}

// OverallDefinition configures aggregation across subsystems.
type OverallDefinition struct {
	Policy AggregationPolicy
}

// Observation is normalized telemetry input for one metric definition.
type Observation struct {
	EvidenceID string
	Value      *Value
	Window     ObservationWindow
	DataAsOf   *time.Time
	Error      ReasonCode
}

// EvaluateMetric turns one observation into a fail-closed metric verdict.
func EvaluateMetric(def MetricDefinition, observation Observation, now time.Time) MetricVerdict {
	result := MetricVerdict{
		ID: def.ID, DisplayName: def.DisplayName, SubsystemID: def.SubsystemID,
		Requirement: def.Requirement, EvidenceID: observation.EvidenceID,
		Criterion: def.Criterion, DisplayPrecision: def.DisplayPrecision,
		ObservationWindow: def.ObservationWindow,
		RequiredFreshness: def.RequiredFreshness.String(),
		Verdict:           VerdictUnknown, ReasonCode: ReasonMissing,
	}
	validObservationWindow := !observation.Window.Start.IsZero() && observation.Window.End.After(observation.Window.Start)
	if validObservationWindow {
		result.ObservationWindow = observation.Window
	}
	if observation.Error != "" {
		switch observation.Error {
		case ReasonMissing, ReasonQueryError, ReasonSchemaError, ReasonUnitError, ReasonInsufficientEvidence:
			result.ReasonCode = observation.Error
		default:
			result.ReasonCode = ReasonSchemaError
		}
		return result
	}
	if observation.Value == nil || observation.DataAsOf == nil {
		return result
	}
	if observation.EvidenceID == "" {
		result.ReasonCode = ReasonInsufficientEvidence
		return result
	}
	if !validObservationWindow {
		result.ReasonCode = ReasonSchemaError
		return result
	}
	age := now.Sub(*observation.DataAsOf)
	if age < 0 {
		result.ReasonCode = ReasonSchemaError
		return result
	}
	result.DataAsOf = observation.DataAsOf
	result.Age = age.String()
	if def.RequiredFreshness <= 0 {
		result.ReasonCode = ReasonSchemaError
		return result
	}
	if !finite(observation.Value.Number) || !criterionFinite(def.Criterion) {
		result.ReasonCode = ReasonSchemaError
		return result
	}
	if observation.Value.Type != ValueNumber && observation.Value.Type != ValueInteger {
		result.ReasonCode = ReasonSchemaError
		return result
	}
	if observation.Value.Type == ValueInteger && math.Trunc(observation.Value.Number) != observation.Value.Number {
		result.ReasonCode = ReasonSchemaError
		return result
	}
	if observation.Value.Unit == "" || observation.Value.Unit != def.Criterion.Unit {
		result.ReasonCode = ReasonUnitError
		return result
	}
	result.Value = observation.Value
	if age > def.RequiredFreshness {
		result.ReasonCode = ReasonStale
		return result
	}
	satisfied, ok := compare(observation.Value.Number, def.Criterion)
	if !ok {
		result.Value = nil
		result.ReasonCode = ReasonSchemaError
		return result
	}
	if satisfied {
		result.Verdict = VerdictGo
		result.ReasonCode = ReasonSatisfied
	} else {
		result.Verdict = VerdictNoGo
		result.ReasonCode = ReasonThresholdViolated
	}
	return result
}

// Build evaluates and deterministically aggregates a complete artifact.
func Build(now time.Time, evidence []EvidenceRef, metricDefinitions []MetricDefinition, observations map[string]Observation, subsystemDefinitions []SubsystemDefinition, overall OverallDefinition) (Artifact, error) {
	artifact := Artifact{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   now.UTC(),
		Evidence:      append([]EvidenceRef{}, evidence...),
		Metrics:       []MetricVerdict{},
		Subsystems:    []SubsystemVerdict{},
		Overall: OverallVerdict{
			RequiredSubsystemIDs: []string{},
			AdvisorySubsystemIDs: []string{},
		},
	}
	for _, def := range metricDefinitions {
		artifact.Metrics = append(artifact.Metrics, EvaluateMetric(def, observations[def.ID], now))
	}
	for _, def := range subsystemDefinitions {
		subsystem := SubsystemVerdict{
			ID: def.ID, DisplayName: def.DisplayName, Requirement: def.Requirement, Policy: def.Policy,
			RequiredMetricIDs: []string{}, AdvisoryMetricIDs: []string{},
		}
		var requiredVerdicts []Verdict
		for _, metric := range artifact.Metrics {
			if metric.SubsystemID != def.ID {
				continue
			}
			if metric.Requirement == Required {
				subsystem.RequiredMetricIDs = append(subsystem.RequiredMetricIDs, metric.ID)
				requiredVerdicts = append(requiredVerdicts, metric.Verdict)
			} else {
				subsystem.AdvisoryMetricIDs = append(subsystem.AdvisoryMetricIDs, metric.ID)
			}
		}
		subsystem.Verdict, subsystem.ReasonCode = aggregate(requiredVerdicts, def.Policy)
		artifact.Subsystems = append(artifact.Subsystems, subsystem)
	}
	var requiredVerdicts []Verdict
	for _, subsystem := range artifact.Subsystems {
		if subsystem.Requirement == Required {
			artifact.Overall.RequiredSubsystemIDs = append(artifact.Overall.RequiredSubsystemIDs, subsystem.ID)
			requiredVerdicts = append(requiredVerdicts, subsystem.Verdict)
		} else {
			artifact.Overall.AdvisorySubsystemIDs = append(artifact.Overall.AdvisorySubsystemIDs, subsystem.ID)
		}
	}
	artifact.Overall.Policy = overall.Policy
	artifact.Overall.Verdict, artifact.Overall.ReasonCode = aggregate(requiredVerdicts, overall.Policy)
	canonicalize(&artifact)
	if err := artifact.Validate(); err != nil {
		return Artifact{}, err
	}
	return artifact, nil
}

func aggregate(required []Verdict, policy AggregationPolicy) (Verdict, ReasonCode) {
	if len(required) == 0 {
		return VerdictUnknown, ReasonInsufficientEvidence
	}
	if slices.Contains(required, VerdictNoGo) {
		return VerdictNoGo, ReasonRequiredNoGo
	}
	if slices.Contains(required, VerdictUnknown) {
		if policy.Unknown == UnknownAllowsGo {
			return VerdictGo, ReasonUnknownAllowed
		}
		return VerdictUnknown, ReasonRequiredUnknown
	}
	return VerdictGo, ReasonAllRequiredGo
}

func compare(value float64, criterion Criterion) (bool, bool) {
	switch criterion.Comparator {
	case GreaterThan:
		return criterion.Threshold != nil && value > *criterion.Threshold, criterion.Threshold != nil
	case GreaterOrEqual:
		return criterion.Threshold != nil && value >= *criterion.Threshold, criterion.Threshold != nil
	case LessThan:
		return criterion.Threshold != nil && value < *criterion.Threshold, criterion.Threshold != nil
	case LessOrEqual:
		return criterion.Threshold != nil && value <= *criterion.Threshold, criterion.Threshold != nil
	case InclusiveRange:
		return criterion.Minimum != nil && criterion.Maximum != nil &&
				value >= *criterion.Minimum && value <= *criterion.Maximum,
			criterion.Minimum != nil && criterion.Maximum != nil && *criterion.Minimum <= *criterion.Maximum
	default:
		return false, false
	}
}

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,127}$`)

// Validate checks semantic constraints that JSON Schema cannot express.
func (artifact Artifact) Validate() error {
	if err := validateArtifactMetadata(artifact); err != nil {
		return err
	}
	evidenceIDs, err := validateEvidence(artifact.Evidence)
	if err != nil {
		return err
	}
	subsystemIDs, err := validateSubsystemDefinitions(artifact.Subsystems)
	if err != nil {
		return err
	}
	metricsByID, err := validateMetrics(artifact.Metrics, subsystemIDs, evidenceIDs, artifact.GeneratedAt)
	if err != nil {
		return err
	}
	if err := validateSubsystems(artifact.Subsystems, metricsByID); err != nil {
		return err
	}
	return validateOverall(artifact.Overall, artifact.Subsystems)
}

func validateArtifactMetadata(artifact Artifact) error {
	if artifact.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schemaVersion must be %q", SchemaVersion)
	}
	if artifact.GeneratedAt.IsZero() {
		return errors.New("generatedAt is required")
	}
	return nil
}

func validateEvidence(evidenceRefs []EvidenceRef) (map[string]struct{}, error) {
	evidenceIDs := make(map[string]struct{}, len(evidenceRefs))
	for i, evidence := range evidenceRefs {
		if err := uniqueID("evidence", i, evidence.ID, evidenceIDs); err != nil {
			return nil, err
		}
		if evidence.URI == "" || evidence.Digest == "" {
			return nil, fmt.Errorf("evidence[%d]: uri and digest are required", i)
		}
	}
	return evidenceIDs, nil
}

func validateSubsystemDefinitions(subsystems []SubsystemVerdict) (map[string]struct{}, error) {
	subsystemIDs := make(map[string]struct{}, len(subsystems))
	for i, subsystem := range subsystems {
		if err := uniqueID("subsystems", i, subsystem.ID, subsystemIDs); err != nil {
			return nil, err
		}
		if subsystem.DisplayName == "" {
			return nil, fmt.Errorf("subsystems[%d]: displayName is required", i)
		}
		if subsystem.Requirement != Required && subsystem.Requirement != Advisory {
			return nil, fmt.Errorf("subsystems[%d]: invalid requirement %q", i, subsystem.Requirement)
		}
	}
	return subsystemIDs, nil
}

func validateMetrics(metrics []MetricVerdict, subsystemIDs, evidenceIDs map[string]struct{}, generatedAt time.Time) (map[string]MetricVerdict, error) {
	metricIDs := make(map[string]struct{}, len(metrics))
	metricsByID := make(map[string]MetricVerdict, len(metrics))
	for i, metric := range metrics {
		if err := uniqueID("metrics", i, metric.ID, metricIDs); err != nil {
			return nil, err
		}
		metricsByID[metric.ID] = metric
		if _, ok := subsystemIDs[metric.SubsystemID]; !ok {
			return nil, fmt.Errorf("metrics[%d]: dangling subsystemId %q", i, metric.SubsystemID)
		}
		if metric.EvidenceID != "" {
			if _, ok := evidenceIDs[metric.EvidenceID]; !ok {
				return nil, fmt.Errorf("metrics[%d]: dangling evidenceId %q", i, metric.EvidenceID)
			}
		}
		if err := validateMetric(metric, generatedAt); err != nil {
			return nil, fmt.Errorf("metrics[%d]: %w", i, err)
		}
	}
	return metricsByID, nil
}

func validateSubsystems(subsystems []SubsystemVerdict, metricsByID map[string]MetricVerdict) error {
	for i, subsystem := range subsystems {
		if err := validateAggregation(subsystem.Verdict, subsystem.ReasonCode, subsystem.Policy); err != nil {
			return fmt.Errorf("subsystems[%d]: %w", i, err)
		}
		if err := validateMembers(subsystem.ID, subsystem.RequiredMetricIDs, subsystem.AdvisoryMetricIDs, metricsByID, func(metric MetricVerdict) (string, Requirement) {
			return metric.SubsystemID, metric.Requirement
		}); err != nil {
			return fmt.Errorf("subsystems[%d]: %w", i, err)
		}
		requiredVerdicts := make([]Verdict, 0, len(subsystem.RequiredMetricIDs))
		for _, id := range subsystem.RequiredMetricIDs {
			requiredVerdicts = append(requiredVerdicts, metricsByID[id].Verdict)
		}
		verdict, reason := aggregate(requiredVerdicts, subsystem.Policy)
		if subsystem.Verdict != verdict || subsystem.ReasonCode != reason {
			return fmt.Errorf("subsystems[%d]: aggregate is %q/%q, want %q/%q", i, subsystem.Verdict, subsystem.ReasonCode, verdict, reason)
		}
	}
	return nil
}

func validateOverall(overall OverallVerdict, subsystems []SubsystemVerdict) error {
	subsystemsByID := make(map[string]SubsystemVerdict, len(subsystems))
	for _, subsystem := range subsystems {
		subsystemsByID[subsystem.ID] = subsystem
	}
	if err := validateAggregation(overall.Verdict, overall.ReasonCode, overall.Policy); err != nil {
		return fmt.Errorf("overall: %w", err)
	}
	if err := validateMembers("", overall.RequiredSubsystemIDs, overall.AdvisorySubsystemIDs, subsystemsByID, func(subsystem SubsystemVerdict) (string, Requirement) {
		return "", subsystem.Requirement
	}); err != nil {
		return fmt.Errorf("overall: %w", err)
	}
	requiredVerdicts := make([]Verdict, 0, len(overall.RequiredSubsystemIDs))
	for _, id := range overall.RequiredSubsystemIDs {
		requiredVerdicts = append(requiredVerdicts, subsystemsByID[id].Verdict)
	}
	verdict, reason := aggregate(requiredVerdicts, overall.Policy)
	if overall.Verdict != verdict || overall.ReasonCode != reason {
		return fmt.Errorf("overall: aggregate is %q/%q, want %q/%q", overall.Verdict, overall.ReasonCode, verdict, reason)
	}
	return nil
}

func validateMetric(metric MetricVerdict, generatedAt time.Time) error {
	if err := validateMetricMetadata(metric); err != nil {
		return err
	}
	freshness, err := validateMetricCriterion(metric)
	if err != nil {
		return err
	}
	if err := validateMetricValue(metric); err != nil {
		return err
	}
	age, err := validateMetricTiming(metric, generatedAt)
	if err != nil {
		return err
	}
	if err := validateMetricStatus(metric); err != nil {
		return err
	}
	return validateMetricObservation(metric, age, freshness)
}

func validateMetricMetadata(metric MetricVerdict) error {
	if metric.DisplayName == "" || metric.SubsystemID == "" {
		return errors.New("displayName and subsystemId are required")
	}
	if metric.Requirement != Required && metric.Requirement != Advisory {
		return fmt.Errorf("invalid requirement %q", metric.Requirement)
	}
	if metric.DisplayPrecision < 0 || metric.DisplayPrecision > 12 {
		return errors.New("displayPrecision must be between 0 and 12")
	}
	if metric.ObservationWindow.Start.IsZero() || !metric.ObservationWindow.End.After(metric.ObservationWindow.Start) {
		return errors.New("observationWindow must have a non-zero start before end")
	}
	return nil
}

func validateMetricCriterion(metric MetricVerdict) (time.Duration, error) {
	if !criterionFinite(metric.Criterion) {
		return 0, errors.New("criterion contains NaN or infinity")
	}
	if !validCriterionShape(metric.Criterion) {
		return 0, errors.New("criterion has an invalid comparator or threshold shape")
	}
	freshness, err := time.ParseDuration(metric.RequiredFreshness)
	if err != nil || freshness <= 0 {
		return 0, errors.New("requiredFreshness must be a positive duration")
	}
	return freshness, nil
}

func validateMetricValue(metric MetricVerdict) error {
	if metric.Value != nil {
		if !finite(metric.Value.Number) {
			return errors.New("value contains NaN or infinity")
		}
		if metric.Value.Type != ValueNumber && metric.Value.Type != ValueInteger {
			return fmt.Errorf("invalid value type %q", metric.Value.Type)
		}
		if metric.Value.Type == ValueInteger && math.Trunc(metric.Value.Number) != metric.Value.Number {
			return errors.New("integer value must be integral")
		}
		if metric.Value.Unit == "" || metric.Value.Unit != metric.Criterion.Unit {
			return errors.New("value unit is incompatible with criterion unit")
		}
	}
	return nil
}

func validateMetricTiming(metric MetricVerdict, generatedAt time.Time) (time.Duration, error) {
	var age time.Duration
	var err error
	if metric.Age != "" {
		age, err = time.ParseDuration(metric.Age)
		if err != nil || age < 0 {
			return 0, errors.New("age must be a non-negative duration")
		}
		if metric.DataAsOf == nil || generatedAt.Sub(*metric.DataAsOf) != age {
			return 0, errors.New("age must equal generatedAt minus dataAsOf")
		}
	} else if metric.DataAsOf != nil {
		return 0, errors.New("dataAsOf requires calculated age")
	}
	return age, nil
}

func validateMetricStatus(metric MetricVerdict) error {
	if !validVerdict(metric.Verdict) {
		return fmt.Errorf("invalid verdict %q", metric.Verdict)
	}
	if !validReasonCode(metric.ReasonCode) {
		return fmt.Errorf("invalid reasonCode %q", metric.ReasonCode)
	}
	if !validMetricReason(metric.Verdict, metric.ReasonCode) {
		return fmt.Errorf("verdict %q is incompatible with reasonCode %q", metric.Verdict, metric.ReasonCode)
	}
	return nil
}

func validMetricReason(verdict Verdict, reason ReasonCode) bool {
	switch verdict {
	case VerdictGo:
		return reason == ReasonSatisfied
	case VerdictNoGo:
		return reason == ReasonThresholdViolated
	case VerdictUnknown:
		return slices.Contains([]ReasonCode{
			ReasonMissing, ReasonStale, ReasonQueryError, ReasonSchemaError, ReasonUnitError, ReasonInsufficientEvidence,
		}, reason)
	default:
		return false
	}
}

func validateMetricObservation(metric MetricVerdict, age, freshness time.Duration) error {
	if err := validateConclusiveMetricObservation(metric, age, freshness); err != nil {
		return err
	}
	if err := validateStaleMetricObservation(metric, age, freshness); err != nil {
		return err
	}
	if err := validateMetricWithoutCanonicalValue(metric); err != nil {
		return err
	}
	return validateMissingMetricObservation(metric)
}

func validateConclusiveMetricObservation(metric MetricVerdict, age, freshness time.Duration) error {
	if metric.Verdict != VerdictUnknown && (metric.Value == nil || metric.EvidenceID == "" || metric.DataAsOf == nil || metric.Age == "") {
		return errors.New("go and no-go require value, evidenceId, dataAsOf, and age")
	}
	if metric.Verdict == VerdictGo || metric.Verdict == VerdictNoGo {
		satisfied, _ := compare(metric.Value.Number, metric.Criterion)
		if (metric.Verdict == VerdictGo) != satisfied {
			return errors.New("verdict is inconsistent with the observed value and criterion")
		}
		if age > freshness {
			return errors.New("go and no-go require fresh evidence")
		}
	}
	return nil
}

func validateStaleMetricObservation(metric MetricVerdict, age, freshness time.Duration) error {
	if metric.ReasonCode == ReasonStale {
		if metric.Value == nil || metric.EvidenceID == "" || metric.DataAsOf == nil || metric.Age == "" || age <= freshness {
			return errors.New("stale requires a value and evidence older than requiredFreshness")
		}
	}
	return nil
}

func validateMetricWithoutCanonicalValue(metric MetricVerdict) error {
	if slices.Contains([]ReasonCode{ReasonQueryError, ReasonSchemaError, ReasonUnitError}, metric.ReasonCode) && metric.Value != nil {
		return fmt.Errorf("%s must not retain a canonical value", metric.ReasonCode)
	}
	return nil
}

func validateMissingMetricObservation(metric MetricVerdict) error {
	if metric.ReasonCode == ReasonMissing && (metric.Value != nil || metric.DataAsOf != nil || metric.Age != "") {
		return errors.New("missing must not contain an observation")
	}
	return nil
}

func validateAggregation(verdict Verdict, reason ReasonCode, policy AggregationPolicy) error {
	if policy.Unknown != UnknownBlocksGo && policy.Unknown != UnknownAllowsGo {
		return fmt.Errorf("invalid unknown policy %q", policy.Unknown)
	}
	if !validVerdict(verdict) {
		return fmt.Errorf("invalid verdict %q", verdict)
	}
	if !validReasonCode(reason) {
		return fmt.Errorf("invalid reasonCode %q", reason)
	}
	valid := verdict == VerdictGo && (reason == ReasonAllRequiredGo || reason == ReasonUnknownAllowed) ||
		verdict == VerdictNoGo && reason == ReasonRequiredNoGo ||
		verdict == VerdictUnknown && (reason == ReasonRequiredUnknown || reason == ReasonInsufficientEvidence)
	if !valid {
		return fmt.Errorf("verdict %q is incompatible with reasonCode %q", verdict, reason)
	}
	return nil
}

func validateMembers[T any](owner string, required, advisory []string, values map[string]T, identity func(T) (string, Requirement)) error {
	seen := make(map[string]struct{}, len(required)+len(advisory))
	for _, group := range []struct {
		ids         []string
		requirement Requirement
	}{{required, Required}, {advisory, Advisory}} {
		for _, id := range group.ids {
			value, ok := values[id]
			if !ok {
				return fmt.Errorf("dangling member ID %q", id)
			}
			if _, duplicate := seen[id]; duplicate {
				return fmt.Errorf("duplicate member ID %q", id)
			}
			seen[id] = struct{}{}
			parent, requirement := identity(value)
			if parent != owner || requirement != group.requirement {
				return fmt.Errorf("member %q is assigned to the wrong owner or requirement list", id)
			}
		}
	}
	for id, value := range values {
		parent, _ := identity(value)
		if parent == owner {
			if _, ok := seen[id]; !ok {
				return fmt.Errorf("member %q is omitted from required and advisory lists", id)
			}
		}
	}
	return nil
}

func uniqueID(kind string, index int, id string, seen map[string]struct{}) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("%s[%d]: invalid id %q", kind, index, id)
	}
	if _, ok := seen[id]; ok {
		return fmt.Errorf("%s[%d]: duplicate id %q", kind, index, id)
	}
	seen[id] = struct{}{}
	return nil
}

func criterionFinite(criterion Criterion) bool {
	for _, value := range []*float64{criterion.Threshold, criterion.Minimum, criterion.Maximum} {
		if value != nil && !finite(*value) {
			return false
		}
	}
	return criterion.Unit != ""
}

func validCriterionShape(criterion Criterion) bool {
	switch criterion.Comparator {
	case GreaterThan, GreaterOrEqual, LessThan, LessOrEqual:
		return criterion.Threshold != nil && criterion.Minimum == nil && criterion.Maximum == nil
	case InclusiveRange:
		return criterion.Threshold == nil && criterion.Minimum != nil && criterion.Maximum != nil &&
			*criterion.Minimum <= *criterion.Maximum
	default:
		return false
	}
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func validVerdict(verdict Verdict) bool {
	return verdict == VerdictGo || verdict == VerdictNoGo || verdict == VerdictUnknown
}

func validReasonCode(reason ReasonCode) bool {
	return slices.Contains([]ReasonCode{
		ReasonSatisfied,
		ReasonThresholdViolated,
		ReasonMissing,
		ReasonStale,
		ReasonQueryError,
		ReasonSchemaError,
		ReasonUnitError,
		ReasonInsufficientEvidence,
		ReasonRequiredNoGo,
		ReasonRequiredUnknown,
		ReasonAllRequiredGo,
		ReasonUnknownAllowed,
	}, reason)
}

func canonicalize(artifact *Artifact) {
	sort.Slice(artifact.Evidence, func(i, j int) bool { return artifact.Evidence[i].ID < artifact.Evidence[j].ID })
	sort.Slice(artifact.Metrics, func(i, j int) bool { return artifact.Metrics[i].ID < artifact.Metrics[j].ID })
	sort.Slice(artifact.Subsystems, func(i, j int) bool { return artifact.Subsystems[i].ID < artifact.Subsystems[j].ID })
	for i := range artifact.Subsystems {
		sort.Strings(artifact.Subsystems[i].RequiredMetricIDs)
		sort.Strings(artifact.Subsystems[i].AdvisoryMetricIDs)
	}
	sort.Strings(artifact.Overall.RequiredSubsystemIDs)
	sort.Strings(artifact.Overall.AdvisorySubsystemIDs)
}

// RenderFacts renders authoritative facts in stable ID order without a model.
func RenderFacts(artifact Artifact) (string, error) {
	if err := artifact.Validate(); err != nil {
		return "", err
	}
	canonicalize(&artifact)
	var out strings.Builder
	fmt.Fprintf(&out, "MISSION CONTROL: %s (%s)\n", strings.ToUpper(string(artifact.Overall.Verdict)), artifact.Overall.ReasonCode)
	for _, subsystem := range artifact.Subsystems {
		fmt.Fprintf(&out, "SUBSYSTEM %s [%s]: %s (%s)\n", subsystem.DisplayName, subsystem.Requirement, strings.ToUpper(string(subsystem.Verdict)), subsystem.ReasonCode)
		for _, metric := range artifact.Metrics {
			if metric.SubsystemID != subsystem.ID {
				continue
			}
			value := "unavailable"
			if metric.Value != nil {
				value = strconv.FormatFloat(metric.Value.Number, 'f', metric.DisplayPrecision, 64) + " " + metric.Value.Unit
			}
			age := "unavailable"
			if metric.Age != "" {
				age = metric.Age
			}
			dataAsOf := "unavailable"
			if metric.DataAsOf != nil {
				dataAsOf = metric.DataAsOf.UTC().Format(time.RFC3339Nano)
			}
			fmt.Fprintf(
				&out,
				"  METRIC %s [%s]: value=%s; criterion=%s; freshness=age %s, required %s, data-as-of %s; window=%s to %s; %s (%s)\n",
				metric.DisplayName,
				metric.Requirement,
				value,
				renderCriterion(metric.Criterion, metric.DisplayPrecision),
				age,
				metric.RequiredFreshness,
				dataAsOf,
				metric.ObservationWindow.Start.UTC().Format(time.RFC3339Nano),
				metric.ObservationWindow.End.UTC().Format(time.RFC3339Nano),
				strings.ToUpper(string(metric.Verdict)),
				metric.ReasonCode,
			)
		}
	}
	return out.String(), nil
}

func renderCriterion(criterion Criterion, precision int) string {
	format := func(value float64) string {
		return strconv.FormatFloat(value, 'f', precision, 64)
	}
	if criterion.Comparator == InclusiveRange {
		return fmt.Sprintf("%s [%s, %s] %s", criterion.Comparator, format(*criterion.Minimum), format(*criterion.Maximum), criterion.Unit)
	}
	return fmt.Sprintf("%s %s %s", criterion.Comparator, format(*criterion.Threshold), criterion.Unit)
}
