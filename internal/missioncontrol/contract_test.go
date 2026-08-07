package missioncontrol

import (
	"encoding/json"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	testNow      = time.Date(2026, 8, 7, 7, 0, 0, 0, time.UTC)
	testDataAsOf = testNow.Add(-time.Minute)
)

func TestEvaluateMetricComparatorsAndFreshness(t *testing.T) {
	tests := []struct {
		name       string
		comparator Comparator
		value      float64
		threshold  float64
		minimum    float64
		maximum    float64
		dataAsOf   time.Time
		unit       string
		want       Verdict
		reason     ReasonCode
	}{
		{name: "greater", comparator: GreaterThan, value: 11, threshold: 10, dataAsOf: testDataAsOf, unit: "ms", want: VerdictGo, reason: ReasonSatisfied},
		{name: "greater inclusive", comparator: GreaterOrEqual, value: 10, threshold: 10, dataAsOf: testDataAsOf, unit: "ms", want: VerdictGo, reason: ReasonSatisfied},
		{name: "less", comparator: LessThan, value: 10, threshold: 10, dataAsOf: testDataAsOf, unit: "ms", want: VerdictNoGo, reason: ReasonThresholdViolated},
		{name: "less inclusive", comparator: LessOrEqual, value: 10, threshold: 10, dataAsOf: testDataAsOf, unit: "ms", want: VerdictGo, reason: ReasonSatisfied},
		{name: "range", comparator: InclusiveRange, value: 10, minimum: 10, maximum: 20, dataAsOf: testDataAsOf, unit: "ms", want: VerdictGo, reason: ReasonSatisfied},
		{name: "stale", comparator: LessThan, value: 10, threshold: 20, dataAsOf: testNow.Add(-6 * time.Minute), unit: "ms", want: VerdictUnknown, reason: ReasonStale},
		{name: "unit error", comparator: LessThan, value: 10, threshold: 20, dataAsOf: testDataAsOf, unit: "s", want: VerdictUnknown, reason: ReasonUnitError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			criterion := Criterion{Comparator: test.comparator, Unit: "ms"}
			if test.comparator == InclusiveRange {
				criterion.Minimum, criterion.Maximum = &test.minimum, &test.maximum
			} else {
				criterion.Threshold = &test.threshold
			}
			got := EvaluateMetric(metricDefinition("latency", Required, criterion), Observation{
				EvidenceID: "query.latency",
				Value:      &Value{Type: ValueNumber, Number: test.value, Unit: test.unit},
				Window:     testWindow(),
				DataAsOf:   &test.dataAsOf,
			}, testNow)
			if got.Verdict != test.want || got.ReasonCode != test.reason {
				t.Fatalf("EvaluateMetric = %q/%q, want %q/%q", got.Verdict, got.ReasonCode, test.want, test.reason)
			}
			if test.reason == ReasonUnitError && got.Value != nil {
				t.Fatal("incompatible source value must not be retained as a canonical value")
			}
		})
	}
}

func TestErrorsCannotYieldGo(t *testing.T) {
	for _, reason := range []ReasonCode{ReasonMissing, ReasonQueryError, ReasonSchemaError, ReasonUnitError} {
		t.Run(string(reason), func(t *testing.T) {
			observation := Observation{Error: reason}
			if reason == ReasonMissing {
				observation.Error = ""
			}
			got := EvaluateMetric(metricDefinition("latency", Required, lessThan(100, "ms")), observation, testNow)
			if got.Verdict != VerdictUnknown {
				t.Fatalf("verdict = %q, want unknown", got.Verdict)
			}
		})
	}
}

func TestEvaluateMetricRejectsMalformedValue(t *testing.T) {
	tests := []struct {
		name  string
		value Value
	}{
		{name: "unknown type", value: Value{Type: "decimal", Number: 50, Unit: "ms"}},
		{name: "fractional integer", value: Value{Type: ValueInteger, Number: 50.5, Unit: "ms"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			observation := validObservation(test.value.Number, test.value.Unit)
			observation.Value = &test.value
			got := EvaluateMetric(metricDefinition("latency", Required, lessThan(100, "ms")), observation, testNow)
			if got.Verdict != VerdictUnknown || got.ReasonCode != ReasonSchemaError {
				t.Fatalf("EvaluateMetric = %q/%q, want unknown/schema-error", got.Verdict, got.ReasonCode)
			}
			if got.Value != nil {
				t.Fatal("malformed source value must not be retained as a canonical value")
			}
		})
	}
}

func TestBuildNormalizesIncompleteEvidence(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Observation)
		reason ReasonCode
	}{
		{
			name: "empty evidence ID",
			mutate: func(observation *Observation) {
				observation.EvidenceID = ""
			},
			reason: ReasonInsufficientEvidence,
		},
		{
			name: "invalid observation window",
			mutate: func(observation *Observation) {
				observation.Window.End = observation.Window.Start
			},
			reason: ReasonSchemaError,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			definition := metricDefinition("latency", Required, lessThan(100, "ms"))
			observation := validObservation(50, "ms")
			test.mutate(&observation)
			artifact, err := Build(
				testNow,
				[]EvidenceRef{{ID: "query.latency", URI: "artifacts/latency.json", Digest: "sha256:latency"}},
				[]MetricDefinition{definition},
				map[string]Observation{"latency": observation},
				[]SubsystemDefinition{{
					ID: "api", DisplayName: "API", Requirement: Required,
					Policy: AggregationPolicy{Unknown: UnknownBlocksGo},
				}},
				OverallDefinition{Policy: AggregationPolicy{Unknown: UnknownBlocksGo}},
			)
			if err != nil {
				t.Fatalf("Build returned an error: %v", err)
			}
			got := artifact.Metrics[0]
			if got.Verdict != VerdictUnknown || got.ReasonCode != test.reason {
				t.Fatalf("metric verdict = %q/%q, want unknown/%s", got.Verdict, got.ReasonCode, test.reason)
			}
			if got.Value != nil || got.DataAsOf != nil || got.Age != "" {
				t.Fatal("incomplete evidence must not retain observation values or freshness")
			}
			if got.ObservationWindow != definition.ObservationWindow {
				t.Fatal("invalid observation window must fall back to the metric definition")
			}
		})
	}
}

func TestUnknownPolicyMustExplicitlyAllowGo(t *testing.T) {
	metric := metricDefinition("latency", Required, lessThan(100, "ms"))
	subsystem := SubsystemDefinition{
		ID: "api", DisplayName: "API", Requirement: Required,
		Policy: AggregationPolicy{Unknown: UnknownAllowsGo},
	}
	artifact, err := Build(testNow, nil, []MetricDefinition{metric}, nil, []SubsystemDefinition{subsystem}, OverallDefinition{
		Policy: AggregationPolicy{Unknown: UnknownAllowsGo},
	})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Subsystems[0].Verdict != VerdictGo || artifact.Subsystems[0].ReasonCode != ReasonUnknownAllowed ||
		artifact.Overall.Verdict != VerdictGo || artifact.Overall.ReasonCode != ReasonAllRequiredGo {
		t.Fatalf("explicit allow policy did not permit unknown: %+v", artifact)
	}
}

func TestBuildIsIndependentOfInputOrdering(t *testing.T) {
	metrics := []MetricDefinition{
		metricDefinition("latency", Required, lessThan(100, "ms")),
		metricDefinition("errors", Advisory, lessThan(1, "percent")),
	}
	subsystems := []SubsystemDefinition{
		{ID: "api", DisplayName: "API", Requirement: Required, Policy: AggregationPolicy{Unknown: UnknownBlocksGo}},
		{ID: "worker", DisplayName: "Worker", Requirement: Advisory, Policy: AggregationPolicy{Unknown: UnknownBlocksGo}},
	}
	metrics[1].SubsystemID = "worker"
	observations := map[string]Observation{
		"latency": validObservation(50, "ms"),
		"errors":  validObservation(0, "percent"),
	}
	evidence := []EvidenceRef{
		{ID: "query.latency", URI: "artifacts/latency.json", Digest: "sha256:latency"},
		{ID: "query.errors", URI: "artifacts/errors.json", Digest: "sha256:errors"},
	}
	baseline, err := Build(testNow, evidence, metrics, observations, subsystems, OverallDefinition{Policy: AggregationPolicy{Unknown: UnknownBlocksGo}})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(baseline)
	for seed := int64(0); seed < 20; seed++ {
		random := rand.New(rand.NewSource(seed))
		random.Shuffle(len(metrics), func(i, j int) { metrics[i], metrics[j] = metrics[j], metrics[i] })
		random.Shuffle(len(subsystems), func(i, j int) { subsystems[i], subsystems[j] = subsystems[j], subsystems[i] })
		random.Shuffle(len(evidence), func(i, j int) { evidence[i], evidence[j] = evidence[j], evidence[i] })
		got, buildErr := Build(testNow, evidence, metrics, observations, subsystems, OverallDefinition{Policy: AggregationPolicy{Unknown: UnknownBlocksGo}})
		if buildErr != nil {
			t.Fatal(buildErr)
		}
		encoded, _ := json.Marshal(got)
		if string(encoded) != string(want) {
			t.Fatalf("seed %d produced order-dependent artifact", seed)
		}
	}
}

func TestValidationRejectsInvalidArtifacts(t *testing.T) {
	tests := map[string]func(*Artifact){
		"NaN": func(artifact *Artifact) {
			artifact.Metrics[0].Value.Number = math.NaN()
		},
		"infinity": func(artifact *Artifact) {
			artifact.Metrics[0].Criterion.Threshold = floatPointer(math.Inf(1))
		},
		"incompatible unit": func(artifact *Artifact) {
			artifact.Metrics[0].Value.Unit = "s"
		},
		"duplicate metric": func(artifact *Artifact) {
			artifact.Metrics = append(artifact.Metrics, artifact.Metrics[0])
		},
		"dangling evidence": func(artifact *Artifact) {
			artifact.Metrics[0].EvidenceID = "query.absent"
		},
		"unknown metric verdict": func(artifact *Artifact) {
			artifact.Metrics[0].Verdict = "maybe"
		},
		"unknown metric reason code": func(artifact *Artifact) {
			artifact.Metrics[0].ReasonCode = "unexpected"
		},
		"unknown aggregate verdict": func(artifact *Artifact) {
			artifact.Subsystems[0].Verdict = "maybe"
		},
		"unknown aggregate reason code": func(artifact *Artifact) {
			artifact.Overall.ReasonCode = "unexpected"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			artifact := goldenArtifact(t, "all-go")
			mutate(&artifact)
			if err := artifact.Validate(); err == nil {
				t.Fatal("Validate succeeded")
			}
		})
	}
}

func TestGoldenArtifactsAndFacts(t *testing.T) {
	for _, scenario := range []string{"all-go", "metric-no-go", "subsystem-unknown", "mixed-required-advisory", "stale-data"} {
		t.Run(scenario, func(t *testing.T) {
			artifact := goldenArtifact(t, scenario)
			encoded, err := json.MarshalIndent(artifact, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			facts, err := RenderFacts(artifact)
			if err != nil {
				t.Fatal(err)
			}
			got := append(encoded, []byte("\n--- FACTS ---\n"+facts)...)
			path := filepath.Join("testdata", scenario+".golden")
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Fatalf("golden mismatch for %s", scenario)
			}
		})
	}
}

func goldenArtifact(t *testing.T, scenario string) Artifact {
	t.Helper()
	metrics := []MetricDefinition{metricDefinition("latency", Required, lessThan(100, "ms"))}
	observations := map[string]Observation{"latency": validObservation(50, "ms")}
	subsystems := []SubsystemDefinition{{
		ID: "api", DisplayName: "API", Requirement: Required,
		Policy: AggregationPolicy{Unknown: UnknownBlocksGo},
	}}
	evidence := []EvidenceRef{{ID: "query.latency", URI: "artifacts/latency.json", Digest: "sha256:latency"}}

	switch scenario {
	case "all-go":
	case "metric-no-go":
		observations["latency"] = validObservation(150, "ms")
	case "subsystem-unknown":
		observations["latency"] = Observation{
			EvidenceID: "query.latency", Window: testWindow(), Error: ReasonQueryError,
		}
	case "mixed-required-advisory":
		metrics = append(metrics, metricDefinition("saturation", Advisory, lessThan(80, "percent")))
		observations["saturation"] = Observation{
			EvidenceID: "query.saturation", Window: testWindow(), Error: ReasonQueryError,
		}
		evidence = append(evidence, EvidenceRef{
			ID: "query.saturation", URI: "artifacts/saturation.json", Digest: "sha256:saturation",
		})
	case "stale-data":
		stale := validObservation(50, "ms")
		asOf := testNow.Add(-10 * time.Minute)
		stale.DataAsOf = &asOf
		observations["latency"] = stale
	default:
		t.Fatalf("unknown scenario %q", scenario)
	}
	artifact, err := Build(testNow, evidence, metrics, observations, subsystems, OverallDefinition{
		Policy: AggregationPolicy{Unknown: UnknownBlocksGo},
	})
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}

func metricDefinition(id string, requirement Requirement, criterion Criterion) MetricDefinition {
	return MetricDefinition{
		ID: id, DisplayName: strings.ToUpper(id[:1]) + id[1:], SubsystemID: "api",
		Requirement: requirement, Criterion: criterion, DisplayPrecision: 1,
		ObservationWindow: testWindow(), RequiredFreshness: 5 * time.Minute,
	}
}

func validObservation(value float64, unit string) Observation {
	return Observation{
		EvidenceID: "query.latency",
		Value:      &Value{Type: ValueNumber, Number: value, Unit: unit},
		Window:     testWindow(),
		DataAsOf:   &testDataAsOf,
	}
}

func lessThan(threshold float64, unit string) Criterion {
	return Criterion{Comparator: LessThan, Unit: unit, Threshold: &threshold}
}

func testWindow() ObservationWindow {
	return ObservationWindow{Start: testNow.Add(-5 * time.Minute), End: testNow}
}

func floatPointer(value float64) *float64 { return &value }
