package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
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
	if len(harnesses) != 1 || harnesses[0].Checks[0].Code != "harness_probe_unobservable" {
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
