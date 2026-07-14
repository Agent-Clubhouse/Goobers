package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/internal/version"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

// buildTelemetryClient constructs the OTel client that spans the runner walk
// (run/task/gate) and scheduler decisions, writing completed spans under
// RunsDir via JournalSpanExporter (issue #126) — the same run journal
// layout goobers trace/telemetry read back through the rollup. Shared by
// up.go/run.go exactly like buildRunnerConfig; each caller owns calling
// Shutdown on the returned client once it's done driving runs.
func buildTelemetryClient(ctx context.Context, l instance.Layout) (*telemetry.Client, error) {
	return telemetry.New(ctx, telemetry.Config{
		ServiceName:    "goobers",
		ServiceVersion: version.Get().Version,
		SpanExporter:   telemetry.NewJournalSpanExporter(l.RunsDir()),
		Batch:          true,
	})
}

// ingestRunTelemetry incrementally ingests one finished run into the local
// telemetry rollup (issue #127) — internal/telemetry/rollup/ingest.go's own
// doc comment already claimed IngestRun is meant to hook a run's completion
// ("call it once a run finishes"), but nothing in cmd/goobers ever called it;
// every `goobers telemetry`/`trace` query instead paid for a full
// rollup.Rebuild (an os.Remove + full rescan) just to stay correct. Called
// from both up.go (every scheduler-dispatched and resumed run) and run.go
// (the one-shot manual run), regardless of the run's own error, so a failed
// run's errors/stage_attempts still show up in `goobers telemetry errors`.
// Best-effort: the rollup is derived state, never the source of truth, so an
// ingest failure here must never fail the run itself.
func ingestRunTelemetry(db *rollup.DB, l instance.Layout, runID string) {
	if db == nil {
		return
	}
	_ = db.IngestRun(filepath.Join(l.RunsDir(), runID))
}

// repoCloneURL overrides runner.Config.RepoCloneURL when non-nil. It exists
// purely as a test seam (mirrors internal/localscheduler's swappable newRunID)
// so integration tests can point worktree provisioning at a local git fixture
// instead of a real GitHub clone; production leaves it nil and runner.New
// falls back to its own github.com default.
var repoCloneURL func(apiv1.RepoRef) (string, error)

// newAgenticAdapter overrides how buildRunnerConfig constructs the harness
// adapter for an agentic stage when non-nil. It is a test seam (mirroring
// repoCloneURL above) so the CLI-level acceptance check (acceptance_test.go)
// can substitute a fake for the real Copilot CLI subprocess and drive the full
// agentic loop — implement -> reviewer gate -> local-ci — through `goobers
// run`/`up` offline, extending #29's runner-API-level walking skeleton to the
// CLI entrypoint. Production leaves it nil and buildRunnerConfig uses the real
// CopilotAdapter.
var newAgenticAdapter func(gooberName string, envCaps map[string]string) harness.Adapter

// credentialGrantEnv is the environment variable the Copilot CLI reads a
// credentialed capability's token from (internal/harness.CopilotAdapter's
// EnvCapabilities convention — matches internal/harness/copilot_test.go's
// {"repo:push": "GH_TOKEN"} fixture).
const credentialGrantEnv = "GH_TOKEN"

// credentialedCapabilities are the canonical capabilities (internal/capability,
// issue #74) a repo's token can satisfy; telemetry:read needs no credential.
var credentialedCapabilities = []capability.Capability{
	capability.RepoPush, capability.GitHubIssuesWrite, capability.GitHubPRWrite,
}

// buildCredentials builds a Resolver and the capability->ref Grants from
// instance.yaml's configured repos. V0 assumes a single target repo per
// instance (ARCHITECTURE.md §6, gaggle.Spec.Project is singular); the first
// configured repo's token backs every credentialed capability. Multiple
// configured repos with different tokens per capability is a known
// simplification — no existing convention maps a capability to a specific
// repo among several, so this is honest about that rather than guessing.
func buildCredentials(cfg *instance.Config) (*credentials.Resolver, []credentials.Grant, error) {
	refs := make([]credentials.TokenRef, 0, len(cfg.Repos))
	for _, r := range cfg.Repos {
		refs = append(refs, credentials.TokenRef{
			Name: r.Owner + "/" + r.Name,
			Env:  r.Token.Env,
			File: r.Token.File,
		})
	}
	resolver, err := credentials.NewResolver(refs)
	if err != nil {
		return nil, nil, fmt.Errorf("build credential resolver: %w", err)
	}
	var grants []credentials.Grant
	if len(cfg.Repos) > 0 {
		ref := cfg.Repos[0].Owner + "/" + cfg.Repos[0].Name
		for _, c := range credentialedCapabilities {
			grants = append(grants, credentials.Grant{Capability: string(c), Ref: ref})
		}
	}
	return resolver, grants, nil
}

// instructionsPath resolves a goober's Instructions field to an absolute
// file path. Instructions is documented as "relative to the goober
// definition directory" (api/v1alpha1.GooberSpec), which config-as-code
// objects don't retain after instance.LoadConfigDir flattens them into a
// ConfigSet — but every shipped config (internal/instance/starter,
// config-examples/, selfhost/) lays goobers out at the same fixed path, so
// that layout convention is reproduced here rather than widening ConfigSet's
// shape for this one field.
func instructionsPath(configDir string, spec apiv1.GooberSpec, gooberName string) string {
	return filepath.Join(configDir, "gaggles", spec.Gaggle, "goobers", gooberName, spec.Instructions)
}

// buildRunnerConfig assembles the runner.Config the daemon (`goobers up`) and
// `goobers run` share: real worktrees, the real Copilot harness adapter and
// shell executor, credentials scoped to instance.yaml's configured repo(s).
// One Config serves every workflow/run — runner.Runner is not bound to a
// single compiled machine.
func buildRunnerConfig(l instance.Layout, cfg *instance.Config, goobers map[string]apiv1.GooberSpec, tel *telemetry.Client) (runner.Config, error) {
	wtMgr, err := worktree.NewManager(l.WorkcopiesDir())
	if err != nil {
		return runner.Config{}, fmt.Errorf("new worktree manager: %w", err)
	}
	resolver, grants, err := buildCredentials(cfg)
	if err != nil {
		return runner.Config{}, err
	}

	envCaps := make(map[string]string, len(credentialedCapabilities))
	for _, c := range credentialedCapabilities {
		envCaps[string(c)] = credentialGrantEnv
	}

	return runner.Config{
		NewDeterministic: func(rec runner.ArtifactRecorder, reg runner.SecretRegistrar) (invoke.Deterministic, error) {
			injector, err := credentials.NewInjector(resolver, grants, reg)
			if err != nil {
				return nil, err
			}
			return executor.NewShellExecutor(injector, rec)
		},
		NewAgentic: func(gooberName string, rec runner.ArtifactRecorder, reg runner.SecretRegistrar) (invoke.Goober, error) {
			spec, ok := goobers[gooberName]
			if !ok {
				return nil, fmt.Errorf("goober %q not found in config", gooberName)
			}
			injector, err := credentials.NewInjector(resolver, grants, reg)
			if err != nil {
				return nil, err
			}
			instructions, err := os.ReadFile(instructionsPath(l.ConfigDir(), spec, gooberName))
			if err != nil {
				return nil, fmt.Errorf("read goober %q instructions: %w", gooberName, err)
			}
			var adapter harness.Adapter = &harness.CopilotAdapter{Command: []string{"copilot"}, EnvCapabilities: envCaps}
			if newAgenticAdapter != nil {
				adapter = newAgenticAdapter(gooberName, envCaps)
			}
			recorder, ok := rec.(harness.SpanRecorder)
			if !ok {
				return nil, fmt.Errorf("runner artifact recorder does not implement harness.SpanRecorder")
			}
			artifacts, ok := rec.(harness.ArtifactRecorder)
			if !ok {
				return nil, fmt.Errorf("runner artifact recorder does not implement harness.ArtifactRecorder")
			}
			registryScrubber, ok := reg.(journal.Scrubber)
			if !ok {
				return nil, fmt.Errorf("runner secret registrar does not implement journal.Scrubber")
			}
			scrubber := journal.Chain(registryScrubber, journal.NewPatternScrubber())
			return harness.NewExecutor(adapter, injector, recorder, artifacts, scrubber, string(instructions))
		},
		Automated:    gate.NewAutomatedEvaluator(),
		Worktrees:    wtMgr,
		RunsDir:      l.RunsDir(),
		RepoCloneURL: repoCloneURL,
		Telemetry:    tel,
	}, nil
}

// goobersByName indexes set's Goobers by name for workflow.WithGoobers
// admission and NewAgentic's instructions/harness lookup.
func goobersByName(set *instance.ConfigSet) map[string]apiv1.GooberSpec {
	out := make(map[string]apiv1.GooberSpec, len(set.Goobers))
	for _, g := range set.Goobers {
		out[g.Name] = g.Spec
	}
	return out
}

// compiledMachines compiles every workflow in set, admission-checked against
// goobers, keyed by workflow name. WorkflowVersion is registry-assigned
// (per-name monotonic, WF-016); no registry is wired at the instance level
// yet, so this pins version 1 for every workflow, matching run.go's existing
// limitation until a follow-up introduces one.
func compiledMachines(set *instance.ConfigSet, goobers map[string]apiv1.GooberSpec) (map[string]*workflow.Machine, error) {
	const workflowVersion = 1
	machines := make(map[string]*workflow.Machine, len(set.Workflows))
	for i := range set.Workflows {
		wf := &set.Workflows[i]
		m, err := workflow.Compile(workflow.Definition{Name: wf.Name, Version: workflowVersion, Spec: wf.Spec}, workflow.WithGoobers(goobers))
		if err != nil {
			return nil, fmt.Errorf("compile workflow %q: %w", wf.Name, err)
		}
		machines[wf.Name] = m
	}
	return machines, nil
}

// repoRefsByWorkflow resolves each workflow's RepoRef via its Gaggle's
// declared project (apiv1.GaggleSpec.Project) — a workflow only names its
// gaggle, not a repo directly.
func repoRefsByWorkflow(set *instance.ConfigSet) (map[string]apiv1.RepoRef, error) {
	gagglesByName := make(map[string]apiv1.Gaggle, len(set.Gaggles))
	for _, g := range set.Gaggles {
		gagglesByName[g.Name] = g
	}
	refs := make(map[string]apiv1.RepoRef, len(set.Workflows))
	for i := range set.Workflows {
		wf := &set.Workflows[i]
		g, ok := gagglesByName[wf.Spec.Gaggle]
		if !ok {
			return nil, fmt.Errorf("workflow %q references unknown gaggle %q", wf.Name, wf.Spec.Gaggle)
		}
		refs[wf.Name] = g.Spec.Project
	}
	return refs, nil
}
