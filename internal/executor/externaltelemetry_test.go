package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/externaltelemetry"
)

func TestTelemetryQueryExecutorRunsFakePointQuery(t *testing.T) {
	executor, recorder := newTelemetryTestExecutor(t)
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	executor.Now = func() time.Time { return now }
	env := apiv1.InvocationEnvelope{
		Workspace:    t.TempDir(),
		Capabilities: []string{string(capability.TelemetryRead)},
		Inputs: map[string]any{
			InputKind:                    KindExternalTelemetry,
			InputTelemetryConnector:      "fixture",
			InputTelemetryQuery:          "request-count",
			InputTelemetryShape:          "point",
			InputTelemetryWindow:         "15m",
			InputTelemetryParameters:     `{"region":"west"}`,
			InputTelemetryExpectedSchema: `[{"name":"requests","type":"integer","unit":"count"}]`,
			InputTelemetryMaxRows:        "1",
		},
	}
	result, err := executor.Run(context.Background(), env, apiv1.DeterministicRun{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != apiv1.ResultSuccess || result.Outputs[OutputTelemetryDataState] != "present" ||
		result.Outputs[OutputTelemetryValue].(json.Number).String() != "42" {
		t.Fatalf("result = %+v", result)
	}
	if len(result.Artifacts) != 1 {
		t.Fatalf("artifacts = %+v", result.Artifacts)
	}
	if result.Artifacts[0].Integrity != apiv1.IntegrityUnapproved ||
		recorder.integrity[ExternalTelemetryArtifactName] != apiv1.IntegrityUnapproved {
		t.Fatalf("external telemetry integrity = %q, recorded %q", result.Artifacts[0].Integrity, recorder.integrity[ExternalTelemetryArtifactName])
	}
	var artifact externaltelemetry.ResultArtifact
	if err := json.Unmarshal(recorder.recorded[ExternalTelemetryArtifactName], &artifact); err != nil {
		t.Fatal(err)
	}
	if artifact.Window.Start == nil || artifact.Window.End == nil ||
		artifact.Window.End.Sub(*artifact.Window.Start) != 15*time.Minute {
		t.Fatalf("window = %+v", artifact.Window)
	}
	if recorder.maxBytes[ExternalTelemetryArtifactName] != artifact.Metadata.MaxBytes {
		t.Fatalf(
			"persisted byte limit = %d, artifact limit %d",
			recorder.maxBytes[ExternalTelemetryArtifactName],
			artifact.Metadata.MaxBytes,
		)
	}
}

func TestTelemetryQueryExecutorLoadsCheckedInQueryRef(t *testing.T) {
	executor, _ := newTelemetryTestExecutor(t)
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "queries"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "queries", "request-count.kql"), []byte("request-count"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := executor.Run(context.Background(), apiv1.InvocationEnvelope{
		Workspace:    workspace,
		Capabilities: []string{string(capability.TelemetryRead)},
		Inputs: map[string]any{
			InputKind:               KindExternalTelemetry,
			InputTelemetryConnector: "fixture",
			InputTelemetryQueryRef:  "queries/request-count.kql",
			InputTelemetryShape:     "point",
		},
	}, apiv1.DeterministicRun{})
	if err != nil || result.Status != apiv1.ResultSuccess {
		t.Fatalf("Run = %+v, %v", result, err)
	}
}

func TestTelemetryQueryExecutorReturnsTypedFailureArtifact(t *testing.T) {
	executor, recorder := newTelemetryTestExecutor(t)
	result, err := executor.Run(context.Background(), apiv1.InvocationEnvelope{
		Workspace:    t.TempDir(),
		Capabilities: []string{string(capability.TelemetryRead)},
		Inputs: map[string]any{
			InputKind:                    KindExternalTelemetry,
			InputTelemetryConnector:      "fixture",
			InputTelemetryQuery:          "request-count",
			InputTelemetryShape:          "point",
			InputTelemetryExpectedSchema: `[{"name":"requests","type":"string"}]`,
		},
	}, apiv1.DeterministicRun{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != apiv1.ResultFailure || result.Error == nil ||
		result.Error.Code != "external_telemetry_schema_mismatch" ||
		result.Outputs[OutputTelemetryDataState] != "failed" {
		t.Fatalf("result = %+v", result)
	}
	var artifact externaltelemetry.ResultArtifact
	if err := json.Unmarshal(recorder.recorded[ExternalTelemetryArtifactName], &artifact); err != nil {
		t.Fatal(err)
	}
	if artifact.State != externaltelemetry.DataFailed || artifact.Failure == nil ||
		artifact.Failure.Code != "schema_mismatch" {
		t.Fatalf("artifact = %+v", artifact)
	}
}

func TestTelemetryQueryExecutorFailsClosedWithoutCapability(t *testing.T) {
	executor, recorder := newTelemetryTestExecutor(t)
	_, err := executor.Run(context.Background(), apiv1.InvocationEnvelope{
		Workspace: t.TempDir(),
		Inputs: map[string]any{
			InputTelemetryConnector: "fixture",
			InputTelemetryQuery:     "request-count",
		},
	}, apiv1.DeterministicRun{})
	if err == nil || !strings.Contains(err.Error(), string(capability.TelemetryRead)) {
		t.Fatalf("error = %v", err)
	}
	if len(recorder.recorded) != 0 {
		t.Fatalf("artifacts recorded without capability: %v", recorder.recorded)
	}
}

func TestTelemetryQueryExecutorRejectsEscapingQueryRef(t *testing.T) {
	executor, _ := newTelemetryTestExecutor(t)
	_, err := executor.Run(context.Background(), apiv1.InvocationEnvelope{
		Workspace:    t.TempDir(),
		Capabilities: []string{string(capability.TelemetryRead)},
		Inputs: map[string]any{
			InputTelemetryConnector: "fixture",
			InputTelemetryQueryRef:  "../secret.kql",
		},
	}, apiv1.DeterministicRun{})
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("error = %v", err)
	}
}

func TestTelemetryQueryExecutorRejectsTypedInputsFromValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value any
	}{
		{
			name:  "numeric max rows",
			key:   InputTelemetryMaxRows,
			value: json.Number("1"),
		},
		{
			name:  "object parameters",
			key:   InputTelemetryParameters,
			value: map[string]any{"region": "west"},
		},
		{
			name: "array expected columns",
			key:  InputTelemetryExpectedSchema,
			value: []any{
				map[string]any{"name": "requests", "type": "integer"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor, recorder := newTelemetryTestExecutor(t)
			inputs := map[string]any{
				InputKind:               KindExternalTelemetry,
				InputTelemetryConnector: "fixture",
				InputTelemetryQuery:     "request-count",
				test.key:                test.value,
			}
			_, err := executor.Run(context.Background(), apiv1.InvocationEnvelope{
				Workspace:    t.TempDir(),
				Capabilities: []string{string(capability.TelemetryRead)},
				Inputs:       inputs,
			}, apiv1.DeterministicRun{})
			if err == nil || !strings.Contains(err.Error(), test.key) ||
				!strings.Contains(err.Error(), "expected string") {
				t.Fatalf("typed %s error = %v", test.key, err)
			}
			if len(recorder.recorded) != 0 {
				t.Fatalf("typed %s recorded artifacts: %v", test.key, recorder.recorded)
			}
		})
	}
}

func newTelemetryTestExecutor(t *testing.T) (*TelemetryQueryExecutor, *fakeRecorder) {
	t.Helper()
	registry := externaltelemetry.NewRegistry()
	if err := registry.Register(externaltelemetry.FakeFactory{}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Configure(externaltelemetry.ConnectorConfig{
		Name:    "fixture",
		Kind:    externaltelemetry.FakeKind,
		Version: externaltelemetry.FakeVersion,
		Config: json.RawMessage(`{
			"source":"local-fixture",
			"responses":{
				"request-count":{
					"columns":[{"name":"requests","type":"integer","unit":"count"}],
					"rows":[[42]]
				}
			}
		}`),
	}, nil, nil); err != nil {
		t.Fatal(err)
	}
	recorder := newFakeRecorder()
	queryExecutor, err := NewTelemetryQueryExecutor(&externaltelemetry.Host{Registry: registry}, recorder)
	if err != nil {
		t.Fatal(err)
	}
	return queryExecutor, recorder
}
