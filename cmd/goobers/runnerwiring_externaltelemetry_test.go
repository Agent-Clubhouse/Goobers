package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/externaltelemetry"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
)

func TestBuildExternalTelemetryExecutorRunsConfiguredFake(t *testing.T) {
	recorder := &externalTelemetryTestRecorder{}
	deterministic, err := buildExternalTelemetryExecutor(externaltelemetry.Configuration{
		Connectors: []externaltelemetry.ConnectorConfig{{
			Name:    "fixture",
			Kind:    externaltelemetry.FakeKind,
			Version: externaltelemetry.FakeVersion,
			Config: json.RawMessage(`{
				"source":"checked-in",
				"responses":{
					"health":{
						"columns":[{"name":"healthy","type":"boolean"}],
						"rows":[[true]]
					}
				}
			}`),
		}},
	}, recorder, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := deterministic.Run(context.Background(), apiv1.InvocationEnvelope{
		Workspace:    t.TempDir(),
		Capabilities: []string{string(capability.TelemetryRead)},
		Inputs: map[string]any{
			executor.InputKind:               executor.KindExternalTelemetry,
			executor.InputTelemetryConnector: "fixture",
			executor.InputTelemetryQuery:     "health",
			executor.InputTelemetryShape:     "point",
		},
	}, apiv1.DeterministicRun{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != apiv1.ResultSuccess || len(recorder.data) == 0 {
		t.Fatalf("result/artifact = %+v / %s", result, recorder.data)
	}
}

func TestExternalTelemetryFakeFixtureWorkflowRunsThroughLocalRunner(t *testing.T) {
	set, report, err := instance.LoadConfigDir("testdata/external-telemetry-workflow")
	if err != nil {
		t.Fatalf("load fixture workflow: %v (report: %+v)", err, report)
	}
	machines, _, _, err := compiledMachinesWithWarnings(set, map[string]apiv1.GooberSpec{}, nil, nil, false)
	if err != nil {
		t.Fatalf("compile fixture workflow: %v", err)
	}
	identity := localscheduler.WorkflowIdentity{Gaggle: "telemetry-fixture", Workflow: "query-health"}
	machine := machines[identity]
	if machine == nil {
		t.Fatalf("compiled workflows do not contain %+v", identity)
	}
	repos, err := repoRefsByWorkflow(set)
	if err != nil {
		t.Fatalf("resolve fixture repository: %v", err)
	}

	layout := instance.NewLayout(t.TempDir())
	if err := layout.EnsureGaggleRuntime(identity.Gaggle); err != nil {
		t.Fatal(err)
	}
	layout = layout.ForGaggle(identity.Gaggle)
	config := &instance.Config{
		ExternalTelemetry: externaltelemetry.Configuration{
			Connectors: []externaltelemetry.ConnectorConfig{{
				Name:    "fixture",
				Kind:    externaltelemetry.FakeKind,
				Version: externaltelemetry.FakeVersion,
				Config: json.RawMessage(`{
					"source":"checked-in/service-health",
					"responses":{
						"health":{
							"columns":[{"name":"healthy","type":"boolean"}],
							"rows":[[true]]
						}
					}
				}`),
			}},
		},
	}
	runnerConfig, _, err := buildRunnerConfig(runnerCompositionInput{
		Layout:               layout,
		Config:               config,
		Goobers:              map[string]apiv1.GooberSpec{},
		InstructionsByGoober: map[string]string{},
		SharedRegistry:       journal.NewRegistryScrubber(),
		GaggleProject:        repos[identity],
		SandboxPosture:       instance.SandboxDisabled,
	})
	if err != nil {
		t.Fatalf("build runner config: %v", err)
	}
	localRunner, err := runner.New(runnerConfig)
	if err != nil {
		t.Fatal(err)
	}

	const runID = "external-telemetry-fixture"
	result, err := localRunner.Start(context.Background(), runner.StartInput{
		RunID:   runID,
		Machine: machine,
		Gaggle:  identity.Gaggle,
		RepoRef: repos[identity],
	})
	if err != nil {
		t.Fatalf("run fixture workflow: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("fixture workflow phase = %q, want %q", result.Phase, journal.PhaseCompleted)
	}

	reader, err := journal.OpenRead(filepath.Join(layout.RunsDir(), runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type != journal.EventStageFinished || event.Stage != "query-health" {
			continue
		}
		if event.Outputs[executor.OutputTelemetryDataState] != string(externaltelemetry.DataPresent) ||
			event.Outputs[executor.OutputTelemetryValue] != true {
			t.Fatalf("workflow outputs = %+v", event.Outputs)
		}
		if len(event.Artifacts) != 1 {
			t.Fatalf("workflow artifacts = %+v", event.Artifacts)
		}
		data, err := reader.ArtifactBytes(event.Artifacts[0])
		if err != nil {
			t.Fatal(err)
		}
		var artifact externaltelemetry.ResultArtifact
		if err := json.Unmarshal(data, &artifact); err != nil {
			t.Fatal(err)
		}
		if artifact.State != externaltelemetry.DataPresent || len(artifact.Rows) != 1 {
			t.Fatalf("normalized artifact = %+v", artifact)
		}
		return
	}
	t.Fatal("query-health stage output was not journaled")
}

func TestBuildExternalTelemetryRegistryValidatesPluginConfigBeforeRun(t *testing.T) {
	_, err := buildExternalTelemetryRegistry(externaltelemetry.Configuration{
		Connectors: []externaltelemetry.ConnectorConfig{{
			Name:    "fixture",
			Kind:    externaltelemetry.FakeKind,
			Version: externaltelemetry.FakeVersion,
			Config:  json.RawMessage(`{"source":"checked-in","responses":{}}`),
		}},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "at least one response") {
		t.Fatalf("build error = %v", err)
	}
}

func TestBuildExternalTelemetryExecutorRejectsUnregisteredPlugin(t *testing.T) {
	_, err := buildExternalTelemetryExecutor(externaltelemetry.Configuration{
		Connectors: []externaltelemetry.ConnectorConfig{{
			Name:    "metrics",
			Kind:    "organization-plugin",
			Version: "v1",
			Config:  json.RawMessage(`{}`),
		}},
	}, &externalTelemetryTestRecorder{}, nil)
	if err == nil || !strings.Contains(err.Error(), "no plugin registered") {
		t.Fatalf("build error = %v", err)
	}
}

type externalTelemetryTestRecorder struct {
	data      []byte
	integrity apiv1.Integrity
}

func (r *externalTelemetryTestRecorder) RecordArtifact(name string, data []byte) (journal.Ref, error) {
	r.data = append([]byte(nil), data...)
	return journal.Ref{Path: name, Digest: journal.Digest(data), Size: int64(len(data))}, nil
}

func (r *externalTelemetryTestRecorder) RecordArtifactBounded(name string, data []byte, maxBytes int) (journal.Ref, error) {
	return r.RecordArtifactBoundedWithIntegrity(name, data, apiv1.IntegrityDerived, maxBytes)
}

func (r *externalTelemetryTestRecorder) RecordArtifactBoundedWithIntegrity(name string, data []byte, integrity apiv1.Integrity, maxBytes int) (journal.Ref, error) {
	if len(data) > maxBytes {
		return journal.Ref{}, errors.New("artifact exceeds byte limit")
	}
	ref, err := r.RecordArtifact(name, data)
	ref.Integrity = integrity
	r.integrity = integrity
	return ref, err
}
