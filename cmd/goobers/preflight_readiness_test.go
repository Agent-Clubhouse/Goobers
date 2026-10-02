package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/mcpconfig"
	"github.com/goobers/goobers/internal/runtimeplan"
)

func TestRuntimeReadinessSelectsScopedModelSourceWithoutResolution(t *testing.T) {
	cfg := &instance.Config{Credentials: []instance.CredentialGrant{
		{Capability: "agent:model", Token: instance.TokenRef{Env: "UNSCOPED"}},
		{Capability: "agent:model", Harness: "claude-code", Token: instance.TokenRef{Store: "vault/model"}},
	}}
	stages := []runtimePreflightStage{{Name: "work", Goober: "agent", Harness: "claude-code", CredentialCapabilities: []string{"agent:model"}}}
	checks, harnesses, err := runtimePreflightReadiness(context.Background(), cfg, apiv1.GaggleSpec{}, nil, stages, runtimeplan.ObserveProcess(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 1 || checks[0].Name != "vault/model" || checks[0].Code != "credential_source_unobservable" {
		t.Fatalf("%+v", checks)
	}
	if len(harnesses) != 1 || harnesses[0].Checks[len(harnesses[0].Checks)-1].Code != "harness_probe_unobservable" {
		t.Fatalf("%+v", harnesses)
	}
}

type runtimeReadinessRunner struct {
	t     *testing.T
	calls int
}

func (r *runtimeReadinessRunner) Run(ctx context.Context, req harness.ProcessRequest) (harness.ProcessResult, error) {
	r.calls++
	if _, ok := ctx.Deadline(); !ok {
		r.t.Fatal("unbounded probe")
	}
	if req.Command[len(req.Command)-1] != "--version" {
		r.t.Fatalf("unexpected auth/model call: %v", req.Command)
	}
	return harness.ProcessResult{ExitCode: 1, Transcript: []byte("opaque-secret-token")}, errors.New("opaque-secret-token")
}
func TestRuntimeReadinessOptInRedactsJSONAndHuman(t *testing.T) {
	root := writeRuntimePreflightFixture(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runner := &runtimeReadinessRunner{t: t}
	original := runtimeReadinessAdapterFor
	t.Cleanup(func() { runtimeReadinessAdapterFor = original })
	runtimeReadinessAdapterFor = func(_ apiv1.Harness, _ harness.EnvironmentConfig, _ map[string][]string, resolve func(context.Context) (string, error)) (harness.Adapter, error) {
		if resolve != nil {
			t.Fatal("configured credential resolver wired")
		}
		return &harness.CopilotAdapter{Command: []string{executable}, Runner: runner}, nil
	}
	for _, asJSON := range []bool{false, true} {
		args := []string{"--instance", root, "--workflow", "implement", "--check-readiness"}
		if asJSON {
			args = append(args, "--json")
		}
		var stdout, stderr bytes.Buffer
		if code := runRuntimePreflight(args, &stdout, &stderr); code != 1 {
			t.Fatalf("exit=%d err=%s", code, stderr.String())
		}
		if strings.Contains(stdout.String()+stderr.String(), "opaque-secret-token") {
			t.Fatal("probe error leaked")
		}
		if !strings.Contains(stdout.String(), "harness_transport_failed") {
			t.Fatalf("missing classified failure: %s", stdout.String())
		}
		if asJSON {
			var report runtimePreflightReport
			if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			for _, stage := range report.Stages {
				if stage.Identity.Outcome == "supported" {
					t.Fatal("interactive readiness transferred to worker")
				}
			}
		}
	}
	if runner.calls != 2 {
		t.Fatalf("calls=%d", runner.calls)
	}
}

func TestRuntimeReadinessReferenceRepositorySource(t *testing.T) {
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "example", Name: "main", Token: instance.TokenRef{Env: "PRIMARY_TOKEN"}},
		{Provider: "github", Owner: "example", Name: "reference", Token: instance.TokenRef{Keychain: "reference-service"}},
	}}
	gaggle := apiv1.GaggleSpec{Project: apiv1.RepoRef{Owner: "example", Name: "main"}, AdditionalRepos: []apiv1.RepoRef{{Owner: "example", Name: "reference"}}}
	key := credentials.RepoScopedCapability("contents:read", "example", "reference")
	stages := []runtimePreflightStage{{Name: "checkout", CredentialCapabilities: []string{key}}}
	checks, _, err := runtimePreflightReadiness(context.Background(), cfg, gaggle, nil, stages, runtimeplan.ObserveProcess(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 1 || checks[0].Name != "reference-service" || checks[0].Code != "credential_source_unobservable" {
		t.Fatalf("%+v", checks)
	}
}

func TestRuntimeReadinessCLIReportsInvalidConfiguration(t *testing.T) {
	for _, harnessName := range []string{"claude-code", "codex"} {
		for _, invalidKind := range []string{"model", "options"} {
			for _, probe := range []bool{false, true} {
				t.Run(harnessName+invalidKind+fmt.Sprint(probe), func(t *testing.T) {
					root := writeRuntimePreflightFixture(t)
					executable, err := os.Executable()
					if err != nil {
						t.Fatal(err)
					}
					original := runtimeReadinessAdapterFor
					t.Cleanup(func() { runtimeReadinessAdapterFor = original })
					runtimeReadinessAdapterFor = func(h apiv1.Harness, _ harness.EnvironmentConfig, _ map[string][]string, _ func(context.Context) (string, error)) (harness.Adapter, error) {
						runner := &runtimeReadinessRunner{t: t}
						if h == apiv1.HarnessClaudeCode {
							return &harness.ClaudeAdapter{Command: []string{executable}, Runner: runner}, nil
						}
						return &harness.CodexAdapter{Command: []string{executable}, Runner: runner}, nil
					}
					path := filepath.Join(root, "config", "gaggles", "example", "goobers", "coder", "goober.yaml")
					content, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					invalid := strings.ReplaceAll(string(content), "harness: copilot", "harness: "+harnessName)
					// Invalid option names/values must not be reflected through admission errors.
					if invalidKind == "options" {
						invalid = strings.ReplaceAll(invalid, "model: auto", "model: auto\n  harnessOptions:\n    opaque-secret-option: opaque-secret-value")
					} else {
						invalid = strings.ReplaceAll(invalid, "model: auto", `model: " opaque-secret-model "`)
					}
					if err := os.WriteFile(path, []byte(invalid), 0600); err != nil {
						t.Fatal(err)
					}
					args := []string{"--instance", root, "--workflow", "implement", "--json"}
					if probe {
						args = append(args, "--check-readiness")
					}
					var stdout, stderr bytes.Buffer
					if code := runRuntimePreflight(args, &stdout, &stderr); code != 1 {
						t.Fatalf("exit %d stderr=%s", code, stderr.String())
					}
					var report runtimePreflightReport
					if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
						t.Fatalf("missing report: %v stderr=%s", err, stderr.String())
					}
					found := false
					for _, h := range report.Harnesses {
						for _, check := range h.Checks {
							if check.Code == "harness_configuration_mismatch" {
								found = true
							}
						}
					}
					if !found {
						t.Fatalf("missing mismatch: %s", stdout.String())
					}
					var human bytes.Buffer
					printRuntimePreflightReport(&human, report)
					if strings.Contains(stdout.String()+stderr.String()+human.String(), "opaque-secret") {
						t.Fatal("admission diagnostic leaked")
					}
				})
			}
		}
	}
}

func TestRuntimeReadinessIncludesTaskAndGateBYOCredentials(t *testing.T) {
	cfg := &instance.Config{Credentials: []instance.CredentialGrant{
		{MCP: "file", Token: instance.TokenRef{File: filepath.Join(t.TempDir(), "missing")}},
		{MCP: "stored", Harness: "claude-code", Token: instance.TokenRef{Store: "vault/mcp"}},
	}}
	spec := apiv1.GooberSpec{Harness: apiv1.HarnessClaudeCode, MCPServers: []apiv1.MCPServer{{Name: "external", CredentialRefs: []apiv1.MCPCredentialRef{
		{Kind: apiv1.MCPCredentialKindBYO, Ref: "file"},
		{Kind: apiv1.MCPCredentialKindBYO, Ref: "stored"},
		{Kind: apiv1.MCPCredentialKindBYO, Ref: "missing-grant"},
	}}}}
	stages := []runtimePreflightStage{{Name: "task", Kind: "task:agentic", Goober: "agent", Harness: "claude-code"}, {Name: "gate", Kind: "gate:agentic", Goober: "agent", Harness: "claude-code"}}
	checks, _, err := runtimePreflightReadiness(context.Background(), cfg, apiv1.GaggleSpec{}, map[string]apiv1.GooberSpec{"agent": spec}, stages, runtimeplan.ObserveProcess(), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range stages {
		for name, want := range map[string]string{"file": "credential_source_absent", "stored": "credential_source_unobservable", "missing-grant": "credential_source_absent"} {
			found := false
			for _, check := range checks {
				if check.Stage == stage.Name && check.Capability == mcpconfig.BYOCredentialKey(name) {
					found = true
					if check.Code != want {
						t.Fatalf("%+v want %s", check, want)
					}
				}
			}
			if !found {
				t.Fatalf("missing %s credential %s: %+v", stage.Name, name, checks)
			}
		}
	}
}

// The diagnostic fallback must keep instance-owned connector admission even
// when a harness configuration error is converted into a report finding.
func TestRuntimeReadinessPreservesTelemetryConnectorAdmission(t *testing.T) {
	set, report, err := instance.LoadConfigDir("testdata/external-telemetry-workflow")
	if err != nil {
		t.Fatalf("load fixture: %v (%+v)", err, report)
	}
	for _, model := range []string{"auto", " invalid model "} {
		t.Run(model, func(t *testing.T) {
			goobers := map[string]apiv1.GooberSpec{"fixture": {Harness: apiv1.HarnessClaudeCode, Model: model}}
			_, _, _, err := compileRuntimePreflight("testdata/external-telemetry-workflow", set, goobers, nil, &instance.Config{})
			var compileErr *workflowCompileError
			if !errors.As(err, &compileErr) || !strings.Contains(err.Error(), `unknown external telemetry connector "fixture"`) {
				t.Fatalf("preflight bypassed connector admission: %v", err)
			}
		})
	}
}
