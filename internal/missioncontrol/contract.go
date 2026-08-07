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

const SchemaVersion = "goobers.dev/mission-control-verdict/v1alpha1"

type Verdict string

const (
	VerdictGo      Verdict = "go"
	VerdictNoGo    Verdict = "no-go"
	VerdictUnknown Verdict = "unknown"
)

type ReasonCode string

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

type Requirement string

const (
	Required Requirement = "required"
	Advisory Requirement = "advisory"
)

type UnknownPolicy string

const (
	UnknownBlocksGo UnknownPolicy = "block"
	UnknownAllowsGo UnknownPolicy = "allow"
)

type Comparator string

const (
	GreaterThan    Comparator = "gt"
	GreaterOrEqual Comparator = "gte"
	LessThan       Comparator = "lt"
	LessOrEqual    Comparator = "lte"
	InclusiveRange Comparator = "range-inclusive"
)

type ValueType string

const (
	ValueNumber  ValueType = "number"
	ValueInteger ValueType = "integer"
)

type EvidenceRef struct {
	ID     string `json:"id"`
	URI    string `json:"uri"`
	Digest string `json:"digest"`
}

type Value struct {
	Type   ValueType `json:"type"`
	Number float64   `json:"number"`
	Unit   string    `json:"unit"`
}

type Criterion struct {
	Comparator Comparator `json:"comparator"`
	Unit       string     `json:"unit"`
	Threshold  *float64   `json:"threshold,omitempty"`
	Minimum    *float64   `json:"minimum,omitempty"`
	Maximum    *float64   `json:"maximum,omitempty"`
}

type ObservationWindow struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

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

type AggregationPolicy struct {
	Unknown UnknownPolicy `json:"unknown"`
}

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

type OverallVerdict struct {
	Policy               AggregationPolicy `json:"policy"`
	RequiredSubsystemIDs []string          `json:"requiredSubsystemIds"`
	AdvisorySubsystemIDs []string          `json:"advisorySubsystemIds"`
	Verdict              Verdict           `json:"verdict"`
	ReasonCode           ReasonCode        `json:"reasonCode"`
}

type Artifact struct {
	SchemaVersion string             `json:"schemaVersion"`
	GeneratedAt   time.Time          `json:"generatedAt"`
	Evidence      []EvidenceRef      `json:"evidence"`
	Metrics       []MetricVerdict    `json:"metrics"`
	Subsystems    []SubsystemVerdict `json:"subsystems"`
	Overall       OverallVerdict     `json:"overall"`
}

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

type SubsystemDefinition struct {
	ID          string
	DisplayName string
	Requirement Requirement
	Policy      AggregationPolicy
}

type OverallDefinition struct {
	Policy AggregationPolicy
}

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
		ObservationWindow: def.ObservationWindow, DataAsOf: observation.DataAsOf,
		RequiredFreshness: def.RequiredFreshness.String(),
		Verdict:           VerdictUnknown, ReasonCode: ReasonMissing,
	}
	if !observation.Window.Start.IsZero() || !observation.Window.End.IsZero() {
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
	age := now.Sub(*observation.DataAsOf)
	if age < 0 {
		result.ReasonCode = ReasonSchemaError
		return result
	}
	result.Age = age.String()
	if def.RequiredFreshness <= 0 {
		result.ReasonCode = ReasonSchemaError
		return result
	}
	if !finite(observation.Value.Number) || !criterionFinite(def.Criterion) {
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
	if artifact.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schemaVersion must be %q", SchemaVersion)
	}
	if artifact.GeneratedAt.IsZero() {
		return errors.New("generatedAt is required")
	}
	evidenceIDs := make(map[string]struct{}, len(artifact.Evidence))
	for i, evidence := range artifact.Evidence {
		if err := uniqueID("evidence", i, evidence.ID, evidenceIDs); err != nil {
			return err
		}
		if evidence.URI == "" || evidence.Digest == "" {
			return fmt.Errorf("evidence[%d]: uri and digest are required", i)
		}
	}
	subsystemIDs := make(map[string]struct{}, len(artifact.Subsystems))
	for i, subsystem := range artifact.Subsystems {
		if err := uniqueID("subsystems", i, subsystem.ID, subsystemIDs); err != nil {
			return err
		}
		if subsystem.DisplayName == "" {
			return fmt.Errorf("subsystems[%d]: displayName is required", i)
		}
		if subsystem.Requirement != Required && subsystem.Requirement != Advisory {
			return fmt.Errorf("subsystems[%d]: invalid requirement %q", i, subsystem.Requirement)
		}
	}
	metricIDs := make(map[string]struct{}, len(artifact.Metrics))
	metricsByID := make(map[string]MetricVerdict, len(artifact.Metrics))
	for i, metric := range artifact.Metrics {
		if err := uniqueID("metrics", i, metric.ID, metricIDs); err != nil {
			return err
		}
		metricsByID[metric.ID] = metric
		if _, ok := subsystemIDs[metric.SubsystemID]; !ok {
			return fmt.Errorf("metrics[%d]: dangling subsystemId %q", i, metric.SubsystemID)
		}
		if metric.EvidenceID != "" {
			if _, ok := evidenceIDs[metric.EvidenceID]; !ok {
				return fmt.Errorf("metrics[%d]: dangling evidenceId %q", i, metric.EvidenceID)
			}
		}
		if err := validateMetric(metric, artifact.GeneratedAt); err != nil {
			return fmt.Errorf("metrics[%d]: %w", i, err)
		}
	}
	for i, subsystem := range artifact.Subsystems {
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
	subsystemsByID := make(map[string]SubsystemVerdict, len(artifact.Subsystems))
	for _, subsystem := range artifact.Subsystems {
		subsystemsByID[subsystem.ID] = subsystem
	}
	if err := validateAggregation(artifact.Overall.Verdict, artifact.Overall.ReasonCode, artifact.Overall.Policy); err != nil {
		return fmt.Errorf("overall: %w", err)
	}
	if err := validateMembers("", artifact.Overall.RequiredSubsystemIDs, artifact.Overall.AdvisorySubsystemIDs, subsystemsByID, func(subsystem SubsystemVerdict) (string, Requirement) {
		return "", subsystem.Requirement
	}); err != nil {
		return fmt.Errorf("overall: %w", err)
	}
	requiredVerdicts := make([]Verdict, 0, len(artifact.Overall.RequiredSubsystemIDs))
	for _, id := range artifact.Overall.RequiredSubsystemIDs {
		requiredVerdicts = append(requiredVerdicts, subsystemsByID[id].Verdict)
	}
	verdict, reason := aggregate(requiredVerdicts, artifact.Overall.Policy)
	if artifact.Overall.Verdict != verdict || artifact.Overall.ReasonCode != reason {
		return fmt.Errorf("overall: aggregate is %q/%q, want %q/%q", artifact.Overall.Verdict, artifact.Overall.ReasonCode, verdict, reason)
	}
	return nil
}

func validateMetric(metric MetricVerdict, generatedAt time.Time) error {
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
	if !criterionFinite(metric.Criterion) {
		return errors.New("criterion contains NaN or infinity")
	}
	if !validCriterionShape(metric.Criterion) {
		return errors.New("criterion has an invalid comparator or threshold shape")
	}
	freshness, err := time.ParseDuration(metric.RequiredFreshness)
	if err != nil || freshness <= 0 {
		return errors.New("requiredFreshness must be a positive duration")
	}
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
	var age time.Duration
	if metric.Age != "" {
		age, err = time.ParseDuration(metric.Age)
		if err != nil || age < 0 {
			return errors.New("age must be a non-negative duration")
		}
		if metric.DataAsOf == nil || generatedAt.Sub(*metric.DataAsOf) != age {
			return errors.New("age must equal generatedAt minus dataAsOf")
		}
	} else if metric.DataAsOf != nil {
		return errors.New("dataAsOf requires calculated age")
	}
	if metric.Verdict == VerdictGo && metric.ReasonCode != ReasonSatisfied ||
		metric.Verdict == VerdictNoGo && metric.ReasonCode != ReasonThresholdViolated ||
		metric.Verdict == VerdictUnknown && !slices.Contains([]ReasonCode{
			ReasonMissing, ReasonStale, ReasonQueryError, ReasonSchemaError, ReasonUnitError, ReasonInsufficientEvidence,
		}, metric.ReasonCode) {
		return fmt.Errorf("verdict %q is incompatible with reasonCode %q", metric.Verdict, metric.ReasonCode)
	}
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
	if metric.ReasonCode == ReasonStale {
		if metric.Value == nil || metric.EvidenceID == "" || metric.DataAsOf == nil || metric.Age == "" || age <= freshness {
			return errors.New("stale requires a value and evidence older than requiredFreshness")
		}
	}
	if slices.Contains([]ReasonCode{ReasonQueryError, ReasonSchemaError, ReasonUnitError}, metric.ReasonCode) && metric.Value != nil {
		return fmt.Errorf("%s must not retain a canonical value", metric.ReasonCode)
	}
	if metric.ReasonCode == ReasonMissing && (metric.Value != nil || metric.DataAsOf != nil || metric.Age != "") {
		return errors.New("missing must not contain an observation")
	}
	return nil
}

func validateAggregation(verdict Verdict, reason ReasonCode, policy AggregationPolicy) error {
	if policy.Unknown != UnknownBlocksGo && policy.Unknown != UnknownAllowsGo {
		return fmt.Errorf("invalid unknown policy %q", policy.Unknown)
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
			fmt.Fprintf(&out, "  METRIC %s [%s]: %s; %s (%s)\n", metric.DisplayName, metric.Requirement, value, strings.ToUpper(string(metric.Verdict)), metric.ReasonCode)
		}
	}
	return out.String(), nil
}
