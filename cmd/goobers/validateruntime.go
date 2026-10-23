package main

import (
	"fmt"
	"io"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/engineadmission"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/supportmatrix"
	"github.com/goobers/goobers/internal/workflow"
)

func emitConfiguredRuntimeFindings(
	options validateOptions,
	configDir string,
	cfg *instance.Config,
	set *instance.ConfigSet,
	report *validate.Report,
	stdout io.Writer,
	diagnostics *diagnosticCollector,
) int {
	findings := appendRuntimeFindings(options.root, configDir, cfg, set, options.target, report)
	errors := emitRuntimeFindings(stdout, diagnostics, findings)
	if errors == 0 {
		return 0
	}
	pf(stdout, "\nconfiguration is not eligible for %s execution (%d runtime blocker(s))\n", options.target, errors)
	return 1
}

type validateTarget string

const (
	validateTargetLocal validateTarget = "local"
	validateTargetCloud validateTarget = "cloud"
)

type runtimeFinding struct {
	code     validate.WarningCode
	severity validate.Severity
	file     string
	path     string
	workflow string
	message  string
}

type runtimeBlocker struct {
	path  string
	issue int
}

var engineRuntimeBlockers = map[workflow.FeatureID]runtimeBlocker{
	"workflow.spec.parallels":           {path: "spec.parallels", issue: 7753},
	"workflow.spec.parallels.selection": {path: "spec.parallels.selection", issue: 7754},
	"workflow.spec.maps":                {path: "spec.maps", issue: 7755},
	"task.childWorkflows":               {path: "task.childWorkflows", issue: 7758},
	"gate.evaluator.human":              {path: "gate.evaluator.human", issue: 7760},
	"task.limits.maxTokens":             {path: "task.limits.maxTokens", issue: 7761},
	"task.limits.maxCostUSD":            {path: "task.limits.maxCostUSD", issue: 7761},
	"task.experiment":                   {path: "task.experiment", issue: 7763},
}

func appendRuntimeFindings(
	root, configDir string,
	cfg *instance.Config,
	set *instance.ConfigSet,
	target validateTarget,
	report *validate.Report,
) []runtimeFinding {
	if cfg == nil || set == nil {
		return nil
	}
	engineConfigured := cfg.EngineProjectionEnabled()
	var findings []runtimeFinding
	for i := range set.Workflows {
		wf := &set.Workflows[i]
		source, _ := set.WorkflowSource(wf.Spec.Gaggle, wf.Name)
		file := configSourceDiagnosticFile(root, configDir, source)
		def := workflow.Definition{Name: wf.Name, Version: 1, DSLVersion: wf.DSLVersion, Spec: wf.Spec}

		severity := validate.Info
		if engineConfigured {
			severity = validate.Warning
		}
		if target == validateTargetCloud {
			severity = validate.Error
		}
		for _, id := range declaredEngineRuntimeBlockers(wf.Spec) {
			blocker := engineRuntimeBlockers[id]
			feature, ok := workflow.LookupFeature(id)
			if !ok || runtimeLevel(feature, workflow.RuntimeEngine) != workflow.RuntimeUnsupported {
				continue
			}
			findings = append(findings, runtimeFinding{
				code:     validate.RuntimeEngineUnsupported,
				severity: severity,
				file:     file,
				path:     "/" + strings.ReplaceAll(blocker.path, ".", "/"),
				workflow: wf.Name,
				message: fmt.Sprintf(
					"workflow declares engine-unsupported feature %s; engine parity is tracked by #%d",
					blocker.path, blocker.issue,
				),
			})
		}

		if engineConfigured || target == validateTargetCloud {
			placements, pinErr := bootstrap.PinChildStagePlacements(cfg, set, wf.Spec.Gaggle, def)
			if pinErr == nil {
				selfPinned, unpinnedGates := runtimePlacementBlockers(def, placements)
				if len(selfPinned) > 0 || len(unpinnedGates) > 0 {
					findings = append(findings, runtimeFinding{
						code:     validate.RuntimeSelfExecutionRequired,
						severity: runtimePlacementSeverity(target),
						file:     file,
						path:     "/spec",
						workflow: wf.Name,
						message:  runtimePlacementMessage(selfPinned, unpinnedGates),
					})
				}
			}
		}

		if target == validateTargetCloud && wf.DSLVersion != supportmatrix.V3DSLVersion &&
			wf.DSLVersion != supportmatrix.V31DSLVersion {
			findings = append(findings, runtimeFinding{
				code:     validate.RuntimeDSLVersionUnsupported,
				severity: validate.Error,
				file:     file,
				path:     "/dslVersion",
				workflow: wf.Name,
				message: fmt.Sprintf(
					"DSL %s is local-runner-only and cannot run in cloud; migrate with `goobers fix --to 3.0`",
					wf.DSLVersion,
				),
			})
		}
	}
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].workflow != findings[j].workflow {
			return findings[i].workflow < findings[j].workflow
		}
		if findings[i].code != findings[j].code {
			return findings[i].code < findings[j].code
		}
		return findings[i].message < findings[j].message
	})
	for _, finding := range findings {
		report.AddFinding(validate.Issue{
			Code: finding.code, Severity: finding.severity, File: finding.file,
			Kind: "Workflow", Name: finding.workflow, Message: finding.message,
		})
	}
	return findings
}

func runtimePlacementBlockers(def workflow.Definition, placements []engine.PinnedPlacement) ([]string, []string) {
	pinned := make(map[string]bool, len(placements))
	var selfPinned []string
	for _, placement := range placements {
		pinned[placement.Stage] = true
		if placement.Self {
			selfPinned = append(selfPinned, placement.Stage)
		}
	}
	var unpinnedGates []string
	for _, gate := range def.Spec.Gates {
		if gate.Evaluator == apiv1.EvaluatorAgentic && !pinned[gate.Name] {
			unpinnedGates = append(unpinnedGates, gate.Name)
		}
	}
	sort.Strings(selfPinned)
	sort.Strings(unpinnedGates)
	return selfPinned, unpinnedGates
}

func declaredEngineRuntimeBlockers(spec apiv1.WorkflowSpec) []workflow.FeatureID {
	declared := make(map[workflow.FeatureID]bool)
	for _, refusal := range engineadmission.UnsupportedFeatures(spec) {
		if id, ok := runtimeFeatureForPath(refusal.Feature); ok {
			declared[id] = true
		}
	}
	for _, gate := range spec.Gates {
		if gate.Evaluator == apiv1.EvaluatorHuman {
			declared["gate.evaluator.human"] = true
		}
	}
	for _, task := range spec.Tasks {
		if task.Type != apiv1.TaskAgentic || task.Limits == nil {
			continue
		}
		if task.Limits.MaxTokens > 0 {
			declared["task.limits.maxTokens"] = true
		}
		if task.Limits.MaxCostUSD > 0 {
			declared["task.limits.maxCostUSD"] = true
		}
	}
	ids := make([]workflow.FeatureID, 0, len(declared))
	for id := range declared {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func runtimeFeatureForPath(path string) (workflow.FeatureID, bool) {
	for id, blocker := range engineRuntimeBlockers {
		if blocker.path == path {
			return id, true
		}
	}
	return "", false
}

func runtimeLevel(feature workflow.Feature, runtime workflow.Runtime) workflow.RuntimeSupportLevel {
	for _, support := range feature.Runtimes {
		if support.Runtime == runtime {
			return support.Level
		}
	}
	return workflow.RuntimeUnsupported
}

func runtimePlacementSeverity(target validateTarget) validate.Severity {
	if target == validateTargetCloud {
		return validate.Error
	}
	return validate.Warning
}

func runtimePlacementMessage(selfPinned, unpinnedGates []string) string {
	var parts []string
	if len(selfPinned) > 0 {
		parts = append(parts, "self-pinned stages ["+strings.Join(selfPinned, ", ")+"] require control-plane-host execution")
	}
	if len(unpinnedGates) > 0 {
		parts = append(parts, "agentic gates ["+strings.Join(unpinnedGates, ", ")+"] declare no runsOn and require control-plane-host execution")
	}
	return strings.Join(parts, "; ")
}

func emitRuntimeFindings(
	stdout io.Writer,
	diagnostics *diagnosticCollector,
	findings []runtimeFinding,
) int {
	errors := 0
	for _, finding := range findings {
		pln(stdout, validate.CodedWarning{
			Code: finding.code, Severity: finding.severity,
			Scope: "Workflow/" + finding.workflow, Explanation: finding.message,
		}.String())
		diagnostics.add(finding.file, finding.path, string(finding.code), string(finding.severity), finding.message)
		if finding.severity == validate.Error {
			errors++
		}
	}
	return errors
}
