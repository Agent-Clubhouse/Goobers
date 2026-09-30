package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runnersolve"
	"github.com/goobers/goobers/internal/workflow"
)

const runtimePreflightSchemaVersion = 1

type runtimePreflightReport struct {
	SchemaVersion    int                       `json:"schemaVersion"`
	Instance         runtimePreflightInstance  `json:"instance"`
	Workflow         runtimePreflightWorkflow  `json:"workflow"`
	Execution        runtimePreflightExecution `json:"execution"`
	Stages           []runtimePreflightStage   `json:"stages"`
	Checks           []runtimePreflightCheck   `json:"checks"`
	MutationBoundary []string                  `json:"mutationBoundary"`
}

type runtimePreflightInstance struct {
	Path      string `json:"path"`
	ConfigDir string `json:"configDir"`
}

type runtimePreflightWorkflow struct {
	Name         string `json:"name"`
	Gaggle       string `json:"gaggle"`
	DSLVersion   string `json:"dslVersion,omitempty"`
	Source       string `json:"source,omitempty"`
	Digest       string `json:"digest"`
	GooberDigest string `json:"gooberDigest"`
}

type runtimePreflightExecution struct {
	IdentityMode string                  `json:"identityMode"`
	Runner       runtimePreflightRunner  `json:"runner"`
	Source       runtimePreflightFactSrc `json:"source"`
}

type runtimePreflightRunner struct {
	Name         string   `json:"name,omitempty"`
	Kind         string   `json:"kind,omitempty"`
	OS           string   `json:"os,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

type runtimePreflightFactSrc struct {
	Fidelity string `json:"fidelity"`
	Detail   string `json:"detail"`
}

type runtimePreflightStage struct {
	Name                   string                  `json:"name"`
	Kind                   string                  `json:"kind"`
	Goober                 string                  `json:"goober,omitempty"`
	Harness                string                  `json:"harness,omitempty"`
	RequiredCapabilities   []string                `json:"requiredCapabilities,omitempty"`
	CredentialCapabilities []string                `json:"credentialCapabilities,omitempty"`
	RunsOn                 *apiv1.RunsOn           `json:"runsOn,omitempty"`
	Source                 runtimePreflightFactSrc `json:"source"`
}

type runtimePreflightCheck struct {
	Category string                  `json:"category"`
	Code     string                  `json:"code"`
	Outcome  string                  `json:"outcome"`
	Stage    string                  `json:"stage,omitempty"`
	Detail   string                  `json:"detail"`
	Source   runtimePreflightFactSrc `json:"source"`
}

func isRuntimePreflightInvocation(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "--instance" || strings.HasPrefix(arg, "--instance=") ||
			arg == "--workflow" || strings.HasPrefix(arg, "--workflow=") ||
			arg == "--execution-identity" || strings.HasPrefix(arg, "--execution-identity=") ||
			arg == "--json" {
			return true
		}
	}
	return false
}

func runRuntimePreflight(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("preflight", flag.ContinueOnError)
	fs.SetOutput(stderr)
	instanceRoot := fs.String("instance", "", "instance root to inspect")
	workflowName := fs.String("workflow", "", "workflow name to inspect")
	executionIdentity := fs.String("execution-identity", "actual", "runner identity mode to report")
	asJSON := fs.Bool("json", false, "emit the versioned JSON report")
	fs.Usage = helpUsage(stderr, "preflight")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*instanceRoot) == "" || strings.TrimSpace(*workflowName) == "" {
		fs.Usage()
		return 2
	}
	if *executionIdentity != "actual" {
		pf(stderr, "error: unsupported execution identity %q (only \"actual\" is reportable without probing)\n", *executionIdentity)
		return 2
	}
	report, err := buildRuntimePreflightReport(*instanceRoot, *workflowName, *executionIdentity)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if *asJSON {
		var out bytes.Buffer
		encoded, err := json.Marshal(report)
		if err != nil {
			pf(stderr, "error: encode preflight report: %v\n", err)
			return 1
		}
		if err := json.Indent(&out, encoded, "", "  "); err != nil {
			pf(stderr, "error: format preflight report: %v\n", err)
			return 1
		}
		pf(stdout, "%s\n", out.String())
		return 0
	}
	printRuntimePreflightReport(stdout, report)
	return 0
}

func buildRuntimePreflightReport(root, workflowName, identityMode string) (runtimePreflightReport, error) {
	layout := layoutFor(root)
	if _, err := os.Stat(layout.ConfigFile()); err != nil {
		return runtimePreflightReport{}, fmt.Errorf("%s not found (not an instance root; run `goobers init` first)", layout.ConfigFile())
	}
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		return runtimePreflightReport{}, fmt.Errorf("load instance config: %w", err)
	}
	set, report, err := instance.LoadConfigDir(layout.ConfigDir())
	if err != nil {
		if report != nil {
			var detail strings.Builder
			printValidationIssues(&detail, report)
			if strings.TrimSpace(detail.String()) != "" {
				return runtimePreflightReport{}, fmt.Errorf("load config: %w: %s", err, strings.TrimSpace(detail.String()))
			}
		}
		return runtimePreflightReport{}, fmt.Errorf("load config: %w", err)
	}
	goobers := goobersByName(set)
	instructions, err := loadGooberInstructions(layout.ConfigDir(), goobers)
	if err != nil {
		return runtimePreflightReport{}, err
	}
	machines, gooberDigests, resolvedGoobers, _, err := compiledMachinesWithGooberDigestsAndWarnings(
		layout.ConfigDir(),
		set,
		goobers,
		instructions,
		harnessEnvironmentPolicy(cfg.Runner),
		cfg.Runner.HarnessCommand,
		true,
		nil,
	)
	if err != nil {
		return runtimePreflightReport{}, err
	}
	wf, err := selectRuntimePreflightWorkflow(set, workflowName)
	if err != nil {
		return runtimePreflightReport{}, err
	}
	identity := localscheduler.WorkflowIdentity{Gaggle: wf.Spec.Gaggle, Workflow: wf.Name}
	machine := machines[identity]
	if machine == nil {
		return runtimePreflightReport{}, fmt.Errorf("compiled workflow %q in gaggle %q is unavailable", wf.Name, wf.Spec.Gaggle)
	}
	absRoot, err := filepathAbs(layout.Root)
	if err != nil {
		return runtimePreflightReport{}, err
	}
	absConfig, err := filepathAbs(layout.ConfigDir())
	if err != nil {
		return runtimePreflightReport{}, err
	}
	source, _ := set.WorkflowSource(wf.Spec.Gaggle, wf.Name)
	runner := runtimePreflightRunnerFromConfig(cfg)
	gaggle, err := runtimePreflightGaggleSpec(set, wf.Spec.Gaggle)
	if err != nil {
		return runtimePreflightReport{}, err
	}
	placements, err := workflow.StagePlacements(machine.Def, gaggle, resolvedGoobers)
	if err != nil {
		return runtimePreflightReport{}, err
	}
	stages := runtimePreflightStages(wf, resolvedGoobers, placements)
	checks := runtimePreflightChecks(stages)
	return runtimePreflightReport{
		SchemaVersion: runtimePreflightSchemaVersion,
		Instance: runtimePreflightInstance{
			Path:      absRoot,
			ConfigDir: absConfig,
		},
		Workflow: runtimePreflightWorkflow{
			Name:         wf.Name,
			Gaggle:       wf.Spec.Gaggle,
			DSLVersion:   wf.DSLVersion,
			Source:       source,
			Digest:       machine.Digest(),
			GooberDigest: gooberDigests[identity],
		},
		Execution: runtimePreflightExecution{
			IdentityMode: identityMode,
			Runner:       runner,
			Source: runtimePreflightFactSrc{
				Fidelity: "static",
				Detail:   "instance.yaml runner declarations only; no host, provider, credential, sandbox, or harness probe was executed",
			},
		},
		Stages: stages,
		Checks: checks,
		MutationBoundary: []string{
			"loaded instance.yaml",
			"loaded config directory",
			"compiled workflow",
			"computed workflow and goober digests",
			"no provider mutation",
			"no package installation",
			"no repository write",
			"no model execution",
		},
	}, nil
}

func filepathAbs(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", path, err)
	}
	return abs, nil
}

func selectRuntimePreflightWorkflow(set *instance.ConfigSet, name string) (apiv1.Workflow, error) {
	var matches []apiv1.Workflow
	for _, wf := range set.Workflows {
		if wf.Name == name {
			matches = append(matches, wf)
		}
	}
	if len(matches) == 0 {
		return apiv1.Workflow{}, fmt.Errorf("no workflow named %q", name)
	}
	if len(matches) > 1 {
		gaggles := make([]string, 0, len(matches))
		for _, wf := range matches {
			gaggles = append(gaggles, wf.Spec.Gaggle)
		}
		sort.Strings(gaggles)
		return apiv1.Workflow{}, fmt.Errorf("workflow name %q is ambiguous across gaggles %s", name, strings.Join(gaggles, ", "))
	}
	return matches[0], nil
}

func runtimePreflightRunnerFromConfig(cfg *instance.Config) runtimePreflightRunner {
	runners := cfg.ResolvedRunners()
	for _, runner := range runners {
		if runner.Host == instance.RunnerHostSelfName {
			caps := append([]string(nil), runner.Provides.Capabilities...)
			sort.Strings(caps)
			return runtimePreflightRunner{
				Name:         runner.Name,
				Kind:         "self",
				OS:           string(runner.Provides.OS),
				Capabilities: caps,
			}
		}
	}
	return runtimePreflightRunner{Kind: "unobservable"}
}

func runtimePreflightGaggleSpec(set *instance.ConfigSet, name string) (apiv1.GaggleSpec, error) {
	for _, gaggle := range set.Gaggles {
		if gaggle.Name == name {
			return gaggle.Spec, nil
		}
	}
	return apiv1.GaggleSpec{}, fmt.Errorf("workflow gaggle %q is not defined", name)
}

func runtimePreflightStages(wf apiv1.Workflow, goobers map[string]apiv1.GooberSpec, placements []runnersolve.StageRequirement) []runtimePreflightStage {
	requirementsByStage := make(map[string][]string, len(placements))
	for _, placement := range placements {
		requirementsByStage[placement.Stage] = sortedStrings(placement.Capabilities)
	}
	stages := make([]runtimePreflightStage, 0, len(wf.Spec.Tasks)+len(wf.Spec.Gates))
	for _, task := range wf.Spec.Tasks {
		stage := runtimePreflightStage{
			Name:                   task.Name,
			Kind:                   "task:" + string(task.Type),
			Goober:                 task.Goober,
			RequiredCapabilities:   requirementsByStage[task.Name],
			CredentialCapabilities: sortedStrings(task.Capabilities),
			RunsOn:                 task.RunsOn,
			Source: runtimePreflightFactSrc{
				Fidelity: "static",
				Detail:   "workflow task definition",
			},
		}
		if task.Type == apiv1.TaskAgentic {
			if goober, ok := goobers[task.Goober]; ok {
				h := goober.Harness
				if h == "" {
					h = apiv1.HarnessCopilot
				}
				stage.Harness = string(h)
			}
		}
		stages = append(stages, stage)
	}
	for _, gate := range wf.Spec.Gates {
		stage := runtimePreflightStage{
			Name:                 gate.Name,
			Kind:                 "gate:" + string(gate.Evaluator),
			RequiredCapabilities: requirementsByStage[gate.Name],
			RunsOn:               gate.RunsOn,
			Source: runtimePreflightFactSrc{
				Fidelity: "static",
				Detail:   "workflow gate definition",
			},
		}
		if gate.Evaluator == apiv1.EvaluatorAgentic && gate.Agentic != nil {
			stage.Goober = gate.Agentic.Goober
			if goober, ok := goobers[gate.Agentic.Goober]; ok {
				h := goober.Harness
				if h == "" {
					h = apiv1.HarnessCopilot
				}
				stage.Harness = string(h)
			}
		}
		stages = append(stages, stage)
	}
	sort.Slice(stages, func(i, j int) bool { return stages[i].Name < stages[j].Name })
	return stages
}

func runtimePreflightChecks(stages []runtimePreflightStage) []runtimePreflightCheck {
	categories := []struct {
		category string
		code     string
		outcome  string
		detail   string
	}{
		{"authentication", "authentication_unobservable", "unobservable", "credential presence and sign-in state require an external auth probe, which this report intentionally does not run"},
		{"transport", "transport_unobservable", "unobservable", "provider and harness transport reachability require external network probes, which this report intentionally does not run"},
		{"authorization", "authorization_unobservable", "unobservable", "permission checks require provider authorization probes, which this report intentionally does not run"},
		{"unavailable_tool", "tool_unobservable", "unobservable", "tool availability requires host or harness execution, which this report intentionally does not run"},
		{"path_access", "path_access_unobservable", "unobservable", "runtime workspace path access requires runner execution, which this report intentionally does not run"},
		{"sandbox", "sandbox_unobservable", "unobservable", "sandbox enforcement requires runner probing, which this report intentionally does not run"},
		{"capability_mismatch", "capability_mismatch_unobservable", "unobservable", "runner capability satisfaction requires scheduler/runner inventory evaluation beyond this static report"},
		{"cleanup_guarantee", "cleanup_guarantee_unsupported", "unsupported", "cleanup guarantees are not proven by this report contract slice"},
		{"unsupported", "external_probe_unsupported", "unsupported", "external credential, harness, MCP, sandbox, lifecycle, and provider probes are outside this report contract slice"},
		{"unobservable", "unknown_facts_explicit", "unobservable", "unknown facts are explicit and cannot satisfy a required guarantee"},
	}
	checks := make([]runtimePreflightCheck, 0, len(categories)+len(stages))
	for _, c := range categories {
		checks = append(checks, runtimePreflightCheck{
			Category: c.category,
			Code:     c.code,
			Outcome:  c.outcome,
			Detail:   c.detail,
			Source: runtimePreflightFactSrc{
				Fidelity: c.outcome,
				Detail:   "contract boundary",
			},
		})
	}
	for _, stage := range stages {
		if len(stage.RequiredCapabilities) == 0 && stage.RunsOn == nil {
			continue
		}
		checks = append(checks, runtimePreflightCheck{
			Category: "capability_mismatch",
			Code:     "stage_capability_satisfaction_unobservable",
			Outcome:  "unobservable",
			Stage:    stage.Name,
			Detail:   "stage declares runner requirements; this report records them but does not prove a runner satisfies them",
			Source: runtimePreflightFactSrc{
				Fidelity: "static",
				Detail:   "workflow stage declaration",
			},
		})
	}
	return checks
}

func sortedStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func printRuntimePreflightReport(w io.Writer, report runtimePreflightReport) {
	pf(w, "runtime preflight: workflow %s (gaggle %s)\n", report.Workflow.Name, report.Workflow.Gaggle)
	pf(w, "  workflow digest: %s\n", report.Workflow.Digest)
	pf(w, "  goober digest: %s\n", report.Workflow.GooberDigest)
	pf(w, "  execution identity: %s (%s)\n", report.Execution.IdentityMode, report.Execution.Source.Fidelity)
	pf(w, "  stages: %d\n", len(report.Stages))
	for _, stage := range report.Stages {
		parts := []string{stage.Kind}
		if stage.Goober != "" {
			parts = append(parts, "goober="+stage.Goober)
		}
		if stage.Harness != "" {
			parts = append(parts, "harness="+stage.Harness)
		}
		if len(stage.RequiredCapabilities) > 0 {
			parts = append(parts, "required="+strings.Join(stage.RequiredCapabilities, ","))
		}
		pf(w, "    %s: %s\n", stage.Name, strings.Join(parts, " "))
	}
	pf(w, "  checks: %d explicit unsupported/unobservable outcomes; no external probes or mutations performed\n", len(report.Checks))
}
