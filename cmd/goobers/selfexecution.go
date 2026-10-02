package main

import (
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/workflow"
)

func selfExecutionObserver(cfg *instance.Config, tel *telemetry.Client) func(bool) {
	cfg.StartSelfExecutionAccounting()
	tel.SelfExecutionPolicy(cfg.SelfExecutionDenied())
	return func(refused bool) { cfg.ObserveSelfExecution(refused); tel.SelfExecutionObserved(refused) }
}

// selfExecutionMigrationReason enumerates work on the local fallback arm,
// including inventories that do not create placement pins at all.
func selfExecutionMigrationReason(def workflow.Definition) string {
	stages := make([]string, 0, len(def.Spec.Tasks)+len(def.Spec.Gates))
	for _, task := range def.Spec.Tasks {
		stages = append(stages, task.Name)
	}
	for _, gate := range def.Spec.Gates {
		if gate.Evaluator == apiv1.EvaluatorAgentic {
			stages = append(stages, gate.Name)
		}
	}
	if len(stages) == 0 {
		return ""
	}
	sort.Strings(stages)
	return "; workflow work resolves to self: [" + strings.Join(stages, ", ") + "]; configure non-self runners and agentic gate runsOn before enabling placement.selfExecution: deny"
}
