package workflowcluster

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

const (
	// StratifyGaggle groups workflows by their owning gaggle.
	StratifyGaggle = "gaggle"
	// StratifyModel groups workflows or runs by recorded model identifiers.
	StratifyModel = "model"
	// StratifyHarness groups workflows or runs by harness identifiers.
	StratifyHarness = "harness"
	// StratifySkills groups workflows by a digest of their skill set.
	StratifySkills = "skills"
	// StratifyTools groups workflows by a digest of their MCP tool set.
	StratifyTools = "tools"
)

var provenanceAxes = []string{StratifyModel, StratifyHarness, StratifySkills, StratifyTools}

// pointStrata maps each supported axis to point labels. Missing labels are
// intentionally omitted so ConfoundCheck reports them as unlabeled.
type pointStrata map[string]Strata

func configuredStrata(workflows []apiv1.Workflow, goobers []apiv1.Goober) pointStrata {
	result := pointStrata{StratifyGaggle: {}}
	for _, axis := range provenanceAxes {
		result[axis] = Strata{}
	}
	specs := make(map[string]apiv1.GooberSpec, len(goobers))
	for _, goober := range goobers {
		specs[goober.Name] = goober.Spec
	}
	for _, wf := range workflows {
		id := wf.Spec.Gaggle + "/" + wf.Name
		result[StratifyGaggle][id] = wf.Spec.Gaggle
		names := map[string]bool{}
		for _, task := range wf.Spec.Tasks {
			if task.Goober != "" {
				names[task.Goober] = true
			}
		}
		for _, gate := range wf.Spec.Gates {
			if gate.Agentic != nil && gate.Agentic.Goober != "" {
				names[gate.Agentic.Goober] = true
			}
		}
		if len(names) == 0 {
			continue
		}
		models, harnesses, skills, tools := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
		complete := true
		for name := range names {
			spec, ok := specs[name]
			if !ok {
				complete = false
				continue
			}
			if spec.Model == "" {
				complete = false
			} else {
				models[spec.Model] = true
			}
			harness := string(spec.Harness)
			if harness == "" {
				harness = string(apiv1.HarnessCopilot)
			}
			harnesses[harness] = true
			for _, skill := range spec.Skills {
				skills[skill] = true
			}
			for _, tool := range spec.Tools {
				tools["tool:"+tool] = true
			}
			for _, server := range spec.MCPServers {
				tools["mcp:"+server.Name] = true
			}
		}
		if complete {
			result[StratifyModel][id] = setLabel(models, false)
		}
		result[StratifyHarness][id] = setLabel(harnesses, false)
		result[StratifySkills][id] = setLabel(skills, true)
		result[StratifyTools][id] = setLabel(tools, true)
	}
	return result
}

func setLabel(values map[string]bool, digest bool) string {
	joined := strings.Join(sortedKeys(values), "\x00")
	if !digest {
		return strings.ReplaceAll(joined, "\x00", "+")
	}
	sum := sha256.Sum256([]byte(joined))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func behaviorStrata(summaries []BehaviorSummary, axis string) Strata {
	strata := Strata{}
	for _, summary := range summaries {
		if value := summary.Provenance[axis]; value != "" {
			strata[summary.ID] = value
		}
	}
	return strata
}
