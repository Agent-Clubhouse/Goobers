package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/triggerqueue"
)

const workflowValidateChildHelp = childworkflow.ValidationHelp
const workflowValidateChildSynopsis = "  goobers workflow validate-child --gaggle <name> --parent <workflow> --stage <stage> [--backend runner|engine] [--cluster-model <model.json>] [--json] <proposal.yaml> [path]\n                                validate a child proposal against its configured parent\n"

func runWorkflowValidateChild(args []string, stdout, stderr io.Writer) int {
	return childworkflow.RunValidationCLI(args, newCLIFlagSet("workflow validate-child", flag.ContinueOnError), stdout, stderr, childworkflow.OfflineGooberAdmission)
}

const workflowChildAdmissionSynopsis = "  goobers workflow child-admission [--approve <child-id> | --reject <child-id>] [--actor <identity>] [--note <text>] [--json] [path]\n                                Labs: review children held for human admission approval\n"

func runWorkflowChildAdmission(args []string, stdout, stderr io.Writer) int {
	return childworkflow.AdmissionCLI{Open: openChildAdmissionStore, DefaultActor: defaultInterventionActor, Now: time.Now}.
		Run(args, newCLIFlagSet("workflow child-admission", flag.ContinueOnError), stdout, stderr)
}

// openChildAdmissionStore opens the existing local queue; it never creates one.
func openChildAdmissionStore(root string) (childworkflow.AdmissionStore, func(), error) {
	path, err := filepath.Abs(filepath.Join(layoutFor(root).SchedulerDir(), "accepted-triggers.db"))
	if err != nil {
		return nil, nil, err
	}
	if _, err := os.Lstat(path); err != nil {
		return nil, nil, fmt.Errorf("no accepted-trigger queue at %s: %w", path, err)
	}
	queue, err := triggerqueue.Open(path)
	if err != nil {
		return nil, nil, err
	}
	return queue, func() { _ = queue.Close() }, nil
}
