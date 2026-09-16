package telemetry

import (
	"fmt"

	apimetric "go.opentelemetry.io/otel/metric"
)

const (
	metricContractID      = "goobers.telemetry.metrics"
	metricContractVersion = "v1"
	metricContractPath    = "metric-contract-v1.json"

	metricKindCounter       = "counter"
	metricKindGauge         = "gauge"
	metricKindHistogram     = "histogram"
	metricKindUpDownCounter = "up_down_counter"

	metricNumberTypeFloat64 = "float64"
	metricNumberTypeInt64   = "int64"

	metricContractStabilityGA     = "ga"
	metricContractInitialVersion  = "dev"
	resourcePresenceOptional      = "optional"
	resourcePresenceRequired      = "required"
	dimensionValuePatternDefault  = "^[A-Za-z0-9._-]{1,64}$"
	dimensionValuePatternUnit     = "^[A-Za-z0-9._{}\\/%-]{1,64}$"
	dimensionInvalidBehaviorDrop  = "drop_dimension"
	dimensionOverflowBehaviorFold = "fold_to_other"
)

type metricContractDocument struct {
	ID                       string                        `json:"id"`
	Version                  string                        `json:"version"`
	Lifecycle                contractLifecycle             `json:"lifecycle"`
	Compatibility            compatibilityPolicy           `json:"compatibility"`
	DimensionCardinality     dimensionCardinalityPolicy    `json:"dimensionCardinality"`
	ResourceAttributes       []resourceAttributeContract   `json:"resourceAttributes"`
	Metrics                  []metricContract              `json:"metrics"`
	DeploymentSpecificInputs deploymentSpecificInputPolicy `json:"deploymentSpecificInputs"`
}

type contractLifecycle struct {
	Stability    string `json:"stability"`
	SinceVersion string `json:"sinceVersion"`
	Deprecated   bool   `json:"deprecated"`
}

type compatibilityPolicy struct {
	AdditiveMetrics        string   `json:"additiveMetrics"`
	BreakingChangeRequires []string `json:"breakingChangeRequires"`
}

type dimensionValuePolicy struct {
	Attribute string `json:"attribute"`
	Pattern   string `json:"pattern"`
}

type dimensionCardinalityPolicy struct {
	MaxDistinctValuesPerAttributePerProcess int                    `json:"maxDistinctValuesPerAttributePerProcess"`
	MaxValueLength                          int                    `json:"maxValueLength"`
	OverflowValue                           string                 `json:"overflowValue"`
	InvalidValueBehavior                    string                 `json:"invalidValueBehavior"`
	OverflowBehavior                        string                 `json:"overflowBehavior"`
	ValuePolicies                           []dimensionValuePolicy `json:"valuePolicies"`
}

type resourceAttributeContract struct {
	Name      string            `json:"name"`
	Presence  string            `json:"presence"`
	Sources   []string          `json:"sources"`
	Behavior  string            `json:"behavior"`
	Lifecycle contractLifecycle `json:"lifecycle"`
}

type metricContract struct {
	Name              string            `json:"name"`
	Kind              string            `json:"kind"`
	NumberType        string            `json:"numberType"`
	Unit              string            `json:"unit"`
	Description       string            `json:"description"`
	AllowedDimensions []string          `json:"allowedDimensions,omitempty"`
	Lifecycle         contractLifecycle `json:"lifecycle"`
}

type deploymentSpecificInputPolicy struct {
	AzureQueriesIncluded bool `json:"azureQueriesIncluded"`
	ThresholdsIncluded   bool `json:"thresholdsIncluded"`
}

var firstClassMetricRegistry = []metricContract{
	{
		Name:              MetricRunDuration,
		Kind:              metricKindHistogram,
		NumberType:        metricNumberTypeFloat64,
		Unit:              "s",
		Description:       "Workflow run duration.",
		AllowedDimensions: []string{AttrWorkflow, AttrOutcome, AttrErrorCode},
		Lifecycle:         stableContractLifecycle(),
	},
	{
		Name:              MetricRunOutcomes,
		Kind:              metricKindCounter,
		NumberType:        metricNumberTypeInt64,
		Unit:              "{run}",
		Description:       "Finished workflow runs by outcome.",
		AllowedDimensions: []string{AttrWorkflow, AttrOutcome, AttrErrorCode},
		Lifecycle:         stableContractLifecycle(),
	},
	{
		Name:              MetricStageDuration,
		Kind:              metricKindHistogram,
		NumberType:        metricNumberTypeFloat64,
		Unit:              "s",
		Description:       "Workflow stage duration.",
		AllowedDimensions: []string{AttrWorkflow, AttrStage, AttrStageType, AttrModel, AttrOutcome, AttrErrorCode, AttrAttemptKind},
		Lifecycle:         stableContractLifecycle(),
	},
	{
		Name:              MetricStageOutcomes,
		Kind:              metricKindCounter,
		NumberType:        metricNumberTypeInt64,
		Unit:              "{stage}",
		Description:       "Finished workflow stages by outcome.",
		AllowedDimensions: []string{AttrWorkflow, AttrStage, AttrStageType, AttrModel, AttrOutcome, AttrErrorCode, AttrAttemptKind},
		Lifecycle:         stableContractLifecycle(),
	},
	{
		Name:              MetricStageRetries,
		Kind:              metricKindCounter,
		NumberType:        metricNumberTypeInt64,
		Unit:              "{attempt}",
		Description:       "Stage attempts beyond the first.",
		AllowedDimensions: []string{AttrWorkflow, AttrStage, AttrStageType, AttrModel, AttrAttemptKind},
		Lifecycle:         stableContractLifecycle(),
	},
	{
		Name:              MetricGateDecisions,
		Kind:              metricKindCounter,
		NumberType:        metricNumberTypeInt64,
		Unit:              "{decision}",
		Description:       "Evaluated gate decisions.",
		AllowedDimensions: []string{AttrWorkflow, AttrStage, AttrStageType, AttrModel, AttrGateDecision},
		Lifecycle:         stableContractLifecycle(),
	},
	{
		Name:              MetricEscalations,
		Kind:              metricKindCounter,
		NumberType:        metricNumberTypeInt64,
		Unit:              "{escalation}",
		Description:       "Stages and gates escalated to a human.",
		AllowedDimensions: []string{AttrWorkflow, AttrStage, AttrStageType, AttrModel, AttrAttemptKind},
		Lifecycle:         stableContractLifecycle(),
	},
	{
		Name:              MetricRedactionsTotal,
		Kind:              metricKindCounter,
		NumberType:        metricNumberTypeInt64,
		Unit:              "{event}",
		Description:       "Scrub events that removed secret material, separated by layer.",
		AllowedDimensions: []string{MetricAttrRedactionLayer},
		Lifecycle:         stableContractLifecycle(),
	},
	{
		Name:        MetricJournalAppendsDropped,
		Kind:        metricKindCounter,
		NumberType:  metricNumberTypeInt64,
		Unit:        "{event}",
		Description: "Explicitly best-effort instance-journal appends that failed.",
		Lifecycle:   stableContractLifecycle(),
	},
	{
		Name:              MetricWorkActive,
		Kind:              metricKindUpDownCounter,
		NumberType:        metricNumberTypeInt64,
		Unit:              "{span}",
		Description:       "In-flight runs and stages.",
		AllowedDimensions: []string{AttrWorkflow, MetricAttrSpanKind},
		Lifecycle:         stableContractLifecycle(),
	},
	{
		Name:              MetricStageMetricValue,
		Kind:              metricKindHistogram,
		NumberType:        metricNumberTypeFloat64,
		Unit:              "1",
		Description:       "Stage-emitted metrics.jsonl values.",
		AllowedDimensions: []string{AttrWorkflow, AttrStage, AttrStageType, AttrModel, AttrAttemptKind, metricNameAttribute, metricUnitAttribute},
		Lifecycle:         stableContractLifecycle(),
	},
	{
		Name:              EventWorktreeDiskUsage,
		Kind:              metricKindGauge,
		NumberType:        metricNumberTypeInt64,
		Unit:              "By",
		Description:       "Apparent bytes of one managed worktree.",
		AllowedDimensions: []string{AttrStorageOperation},
		Lifecycle:         stableContractLifecycle(),
	},
	{
		Name:              EventWorkcopyDiskUsage,
		Kind:              metricKindGauge,
		NumberType:        metricNumberTypeInt64,
		Unit:              "By",
		Description:       "Aggregate apparent bytes of managed workcopies.",
		AllowedDimensions: []string{AttrStorageOperation},
		Lifecycle:         stableContractLifecycle(),
	},
	{
		Name:              MetricRecoverySnapshotFormat,
		Kind:              metricKindCounter,
		NumberType:        metricNumberTypeInt64,
		Unit:              "{capture}",
		Description:       "Recovery snapshot bundle captures by format selected.",
		AllowedDimensions: []string{MetricAttrRecoverySnapshotFormat},
		Lifecycle:         stableContractLifecycle(),
	},
	{
		Name:              MetricRecoverySnapshotBytes,
		Kind:              metricKindHistogram,
		NumberType:        metricNumberTypeFloat64,
		Unit:              "By",
		Description:       "Emitted recovery snapshot bundle size.",
		AllowedDimensions: []string{MetricAttrRecoverySnapshotFormat},
		Lifecycle:         stableContractLifecycle(),
	},
	{
		Name:              MetricRecoverySnapshotFallback,
		Kind:              metricKindCounter,
		NumberType:        metricNumberTypeInt64,
		Unit:              "{capture}",
		Description:       "Recovery captures that fell back to a full bundle, by reason.",
		AllowedDimensions: []string{MetricAttrRecoveryReason},
		Lifecycle:         stableContractLifecycle(),
	},
	{
		Name:              MetricRecoveryRestoreFailures,
		Kind:              metricKindCounter,
		NumberType:        metricNumberTypeInt64,
		Unit:              "{failure}",
		Description:       "Recovery bundle restore failures, by reason.",
		AllowedDimensions: []string{MetricAttrRecoveryReason},
		Lifecycle:         stableContractLifecycle(),
	},
	{
		Name:        MetricStorageFreeBytes,
		Kind:        metricKindGauge,
		NumberType:  metricNumberTypeInt64,
		Unit:        "By",
		Description: "Most recent free-space sample of the filesystem containing the instance root.",
		Lifecycle:   stableContractLifecycle(),
	},
	{
		Name:              MetricStorageHealthTierChanges,
		Kind:              metricKindCounter,
		NumberType:        metricNumberTypeInt64,
		Unit:              "{transition}",
		Description:       "Tiered low-disk protection tier transitions, tagged by the tier entered.",
		AllowedDimensions: []string{MetricAttrStorageTier},
		Lifecycle:         stableContractLifecycle(),
	},
}

var resourceAttributeRegistry = []resourceAttributeContract{
	{
		Name:      "service.name",
		Presence:  resourcePresenceRequired,
		Sources:   []string{"default:goobers", "Config.ServiceName", "OTEL_SERVICE_NAME"},
		Behavior:  "Always emitted. OTEL_SERVICE_NAME overrides Config.ServiceName and the default value.",
		Lifecycle: stableContractLifecycle(),
	},
	{
		Name:      "service.instance.id",
		Presence:  resourcePresenceRequired,
		Sources:   []string{"generated per client", "OTEL_RESOURCE_ATTRIBUTES service.instance.id"},
		Behavior:  "Always emitted. A fresh value is generated unless OTEL_RESOURCE_ATTRIBUTES supplies one.",
		Lifecycle: stableContractLifecycle(),
	},
	{
		Name:      "service.version",
		Presence:  resourcePresenceOptional,
		Sources:   []string{"Config.ServiceVersion"},
		Behavior:  "Emitted only when Config.ServiceVersion is non-empty.",
		Lifecycle: stableContractLifecycle(),
	},
	{
		Name:      "goobers.build.commit",
		Presence:  resourcePresenceOptional,
		Sources:   []string{"Config.BuildCommit"},
		Behavior:  "Emitted only when Config.BuildCommit is non-empty.",
		Lifecycle: stableContractLifecycle(),
	},
	{
		Name:      "deployment.environment",
		Presence:  resourcePresenceOptional,
		Sources:   []string{"Config.Environment"},
		Behavior:  "Emitted only when Config.Environment is non-empty.",
		Lifecycle: stableContractLifecycle(),
	},
}

func stableContractLifecycle() contractLifecycle {
	return contractLifecycle{
		Stability:    metricContractStabilityGA,
		SinceVersion: metricContractInitialVersion,
		Deprecated:   false,
	}
}

func firstClassMetricSpec(name, kind, numberType string) (metricContract, error) {
	for _, spec := range firstClassMetricRegistry {
		if spec.Name != name {
			continue
		}
		if spec.Kind != kind || spec.NumberType != numberType {
			return metricContract{}, fmt.Errorf("telemetry metric %s registered as %s/%s, want %s/%s", name, spec.Kind, spec.NumberType, kind, numberType)
		}
		return spec, nil
	}
	return metricContract{}, fmt.Errorf("telemetry metric %s is not in the first-class registry", name)
}

func newFloat64Histogram(meter apimetric.Meter, name string) (apimetric.Float64Histogram, error) {
	spec, err := firstClassMetricSpec(name, metricKindHistogram, metricNumberTypeFloat64)
	if err != nil {
		return nil, err
	}
	return meter.Float64Histogram(spec.Name, apimetric.WithUnit(spec.Unit), apimetric.WithDescription(spec.Description))
}

func newInt64Counter(meter apimetric.Meter, name string) (apimetric.Int64Counter, error) {
	spec, err := firstClassMetricSpec(name, metricKindCounter, metricNumberTypeInt64)
	if err != nil {
		return nil, err
	}
	return meter.Int64Counter(spec.Name, apimetric.WithUnit(spec.Unit), apimetric.WithDescription(spec.Description))
}

func newInt64UpDownCounter(meter apimetric.Meter, name string) (apimetric.Int64UpDownCounter, error) {
	spec, err := firstClassMetricSpec(name, metricKindUpDownCounter, metricNumberTypeInt64)
	if err != nil {
		return nil, err
	}
	return meter.Int64UpDownCounter(spec.Name, apimetric.WithUnit(spec.Unit), apimetric.WithDescription(spec.Description))
}

func newInt64Gauge(meter apimetric.Meter, name string) (apimetric.Int64Gauge, error) {
	spec, err := firstClassMetricSpec(name, metricKindGauge, metricNumberTypeInt64)
	if err != nil {
		return nil, err
	}
	return meter.Int64Gauge(spec.Name, apimetric.WithUnit(spec.Unit), apimetric.WithDescription(spec.Description))
}
