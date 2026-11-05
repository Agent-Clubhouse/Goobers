package main

import (
	"flag"
	"io"

	"github.com/goobers/goobers/internal/childworkflow"
)

const workflowValidateChildHelp = childworkflow.ValidationHelp
const workflowValidateChildSynopsis = "  goobers workflow validate-child --gaggle <name> --parent <workflow> --stage <stage> [--backend runner|engine] [--cluster-model <model.json>] [--json] <proposal.yaml> [path]\n                                validate a child proposal against its configured parent\n"

func runWorkflowValidateChild(args []string, stdout, stderr io.Writer) int {
	return childworkflow.RunValidationCLI(args, newCLIFlagSet("workflow validate-child", flag.ContinueOnError), stdout, stderr, childworkflow.OfflineGooberAdmission)
}
