package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/decisiongate"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
	harnesstest "github.com/goobers/goobers/test/testsupport/harness"
)

// TestDecisionShadowRecordsBoundLegacyHandoffValidity drives the whole #6733
// path: decisionGate.handoffSchemas binds a DSL 2.0 producer's JSON result to
// the shipped claimed-item schema, the runner validates the handoff, the
// harness surfaces the report and the shadow observer logs inputValid.
func TestDecisionShadowRecordsBoundLegacyHandoffValidity(t *testing.T) {
	cfg := shadowE2EInstanceConfig(t)
	valid, err := marshalClaimedBacklogItems([]providers.WorkItem{{Provider: providers.ProviderGitHub, ID: "6733", Title: "bind handoff schemas"}}, nil, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, payload, want string
	}{
		{"valid", string(valid), "true"},
		{"invalid", `{"provider":"github","title":"claimed item without an id"}`, "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := &syncBuffer{}
			observer := newDecisionShadowObserver(cfg, slog.New(slog.NewJSONHandler(logs, nil)))
			if observer == nil {
				t.Fatal("shadow observer not installed")
			}
			runShadowE2E(t, cfg, observer, "run-shadow-"+tc.name, tc.payload)
			record := waitForShadowRecord(t, logs)
			if record["inputValid"] != tc.want {
				t.Fatalf("decisiongate.shadow inputValid = %v, want %s; record=%v", record["inputValid"], tc.want, record)
			}
		})
	}
}

func shadowE2EInstanceConfig(t *testing.T) *instance.Config {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Questions map[string]json.RawMessage `json:"questions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		answers := map[string]any{}
		for name := range req.Questions {
			answers[name] = map[string]any{"type": "noul", "noul": 0.1}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "fake", "answers": answers})
	}))
	t.Cleanup(server.Close)
	t.Setenv("GOOBERS_SHADOW_E2E_URL", server.URL)
	t.Setenv("GOOBERS_SHADOW_E2E_KEY", "test-key")
	t.Setenv("GOOBERS_SHADOW_E2E_MODEL", "fake")
	return &instance.Config{DecisionGate: &decisiongate.Settings{
		Mode: decisiongate.ModeShadow, BaseURLEnv: "GOOBERS_SHADOW_E2E_URL", KeyEnv: "GOOBERS_SHADOW_E2E_KEY",
		ModelEnv: "GOOBERS_SHADOW_E2E_MODEL", Fallback: decisiongate.FallbackAgent, ShadowSample: 1,
		HandoffSchemas: []decisiongate.HandoffSchemaBinding{{
			Workflow: "legacy-shadow", Stage: "query-backlog", SchemaPath: "gaggles/goobers/schemas/claimed-item.schema.json",
		}},
	}}
}

func runShadowE2E(t *testing.T, cfg *instance.Config, observer harness.Observer, runID, payload string) {
	t.Helper()
	root := t.TempDir()
	runsDir := filepath.Join(root, "runs")
	resolver, err := credentials.NewResolver(nil)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := worktree.NewManager(filepath.Join(root, "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	rc := runner.Config{
		Worktrees: manager,
		RunsDir:   runsDir, ScratchDir: filepath.Join(root, "scratch"),
		HandoffSchemaLoader: newHandoffSchemaLoader(cfg, "../../reference-workflows"),
		NewDeterministic: func(rec runner.ArtifactRecorder, _ runner.SecretRegistrar) (invoke.Deterministic, error) {
			return shadowE2EProducer{rec: rec, payload: payload}, nil
		},
		NewAgentic: func(gooberName string, rec runner.ArtifactRecorder, registrar runner.SecretRegistrar) (invoke.Goober, error) {
			injector, err := credentials.NewGooberInjector(resolver, gooberName, nil, registrar)
			if err != nil {
				return nil, err
			}
			adapter := &harnesstest.FakeAdapter{
				Transcript: []byte("implemented\n"),
				Act: func(_ context.Context, request harness.RunRequest) error {
					return harnesstest.WriteCompletion(request.Workspace, request.CompletionPath, apiv1.ResultEnvelope{
						Status: apiv1.ResultSuccess, Summary: "implemented the claimed item",
					})
				},
			}
			return harness.NewExecutor(adapter, injector, rec.(harness.SpanRecorder), rec,
				harness.NewContextResolver(rec.(interface{ Dir() string }), runsDir),
				journal.Chain(registrar.(journal.Scrubber), journal.NewPatternScrubber()),
				"shadow e2e fixture", harness.WithObserver(observer))
		},
	}
	applyRunnerConfigFinalizers(&rc, runnerCompositionInput{Config: cfg}, nil)
	r, err := runner.New(rc)
	if err != nil {
		t.Fatal(err)
	}
	machine, err := workflow.Compile(workflow.Definition{Name: "legacy-shadow", Version: 1, DSLVersion: "2.0", Spec: apiv1.WorkflowSpec{
		Gaggle: "acme-web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}}, Start: "query-backlog",
		Tasks: []apiv1.Task{
			{
				Name: "query-backlog", Type: apiv1.TaskDeterministic, Goal: "claim an item", Workspace: apiv1.WorkspaceScratch,
				Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "implement",
			},
			{Name: "implement", Type: apiv1.TaskAgentic, Goal: "implement the item", Goober: "implementer", Workspace: apiv1.WorkspaceScratch, Next: workflow.TerminalComplete},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Start(context.Background(), runner.StartInput{
		RunID: runID, Machine: machine, Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
}

// shadowE2EProducer mimics a DSL 2.0 shell stage: stdout plus its declared
// .json resultFile, without an artifact-set manifest.
type shadowE2EProducer struct {
	rec     runner.ArtifactRecorder
	payload string
}

func (p shadowE2EProducer) Run(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	var artifacts []apiv1.ArtifactPointer
	for _, a := range []struct{ name, media, data string }{
		{"query-backlog/stdout.log", "text/plain", "claimed\n"},
		{"query-backlog/result", "application/json", p.payload},
	} {
		ref, err := p.rec.RecordArtifact(a.name, []byte(a.data))
		if err != nil {
			return apiv1.ResultEnvelope{}, err
		}
		artifacts = append(artifacts, apiv1.ArtifactPointer{Path: ref.Path, Digest: ref.Digest, Size: ref.Size, MediaType: a.media, Integrity: ref.Integrity})
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Artifacts: artifacts}, nil
}

// waitForShadowRecord returns the first decisiongate.shadow log record; the
// observer scores in the background.
func waitForShadowRecord(t *testing.T, logs *syncBuffer) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, line := range strings.Split(logs.String(), "\n") {
			var record map[string]any
			if json.Unmarshal([]byte(line), &record) == nil && record["msg"] == "decisiongate.shadow" {
				return record
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no decisiongate.shadow record logged; logs=%s", logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
