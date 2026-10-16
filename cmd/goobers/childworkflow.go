package main

import (
	"flag"
	"fmt"
	"io"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/instance"
)

const workflowValidateChildHelp = childworkflow.ValidationHelp
const workflowValidateChildSynopsis = "  goobers workflow validate-child --gaggle <name> --parent <workflow> --stage <stage> [--backend runner|engine] [--json] <proposal.yaml> [path]\n                                validate a child proposal against its configured parent\n"

func runWorkflowValidateChild(args []string, stdout, stderr io.Writer) int {
	return childworkflow.RunValidationCLI(args, newCLIFlagSet("workflow validate-child", flag.ContinueOnError), stdout, stderr, admitChildValidationGoobers)
}

func admitChildValidationGoobers(cfg *instance.Config, goobers map[string]apiv1.GooberSpec) (childworkflow.AdmittedGoobers, error) {
	registry, err := buildHarnessRegistry(nil, harnessEnvironmentPolicy(cfg.Runner), cfg.Runner.HarnessCommand, "", "", true, nil, false)
	if err != nil {
		return childworkflow.AdmittedGoobers{}, err
	}
	resolved, warnings, err := admitGooberHarnessConfigs(registry, goobers)
	result := childworkflow.AdmittedGoobers{Goobers: resolved, HarnessNames: registry.Names()}
	for _, warning := range warnings {
		result.Warnings = append(result.Warnings, fmt.Sprintf("Goober/%s: %s", warning.Goober, warning.Warning.Message))
	}
	return result, err
}
