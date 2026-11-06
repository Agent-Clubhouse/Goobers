package main

import (
	"io"
	"text/tabwriter"

	"github.com/goobers/goobers/internal/workflowgym"
)

const workflowGymHelp = "Usage: goobers workflow-gym <validate|list> <path>\n\n" +
	"Validate or list a file-backed Workflow Gym benchmark dataset. The path\n" +
	"may name one JSON/YAML task manifest or a directory of manifests.\n"

const workflowGymValidateHelp = "Usage: goobers workflow-gym validate <path>\n\n" +
	"Validate a Workflow Gym dataset and print its version, deterministic\n" +
	"digest, and task count.\n"

const workflowGymListHelp = "Usage: goobers workflow-gym list <path>\n\n" +
	"Validate a Workflow Gym dataset, print its version, deterministic digest,\n" +
	"and task count, then list tasks in stable task ID order.\n"

func runWorkflowGym(args []string, _, stderr io.Writer) int {
	if len(args) > 0 {
		pf(stderr, "error: unknown workflow-gym command %q\n", args[0])
	}
	pf(stderr, "%s", workflowGymHelp)
	return 2
}

func runWorkflowGymValidate(args []string, stdout, stderr io.Writer) int {
	registry, code := loadWorkflowGymCLI(args, stderr)
	if code != 0 {
		return code
	}
	printWorkflowGymSummary(stdout, registry)
	return 0
}

func runWorkflowGymList(args []string, stdout, stderr io.Writer) int {
	registry, code := loadWorkflowGymCLI(args, stderr)
	if code != 0 {
		return code
	}
	printWorkflowGymSummary(stdout, registry)
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	for _, task := range registry.Tasks {
		pf(tw, "%s\t%s\n", task.TaskID, task.Split)
	}
	if err := tw.Flush(); err != nil {
		pf(stderr, "error: write task list: %v\n", err)
		return 1
	}
	return 0
}

func loadWorkflowGymCLI(args []string, stderr io.Writer) (workflowgym.Registry, int) {
	if len(args) != 1 {
		pf(stderr, "%s", workflowGymHelp)
		return workflowgym.Registry{}, 2
	}
	registry, err := workflowgym.Load(args[0])
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return workflowgym.Registry{}, 1
	}
	return registry, 0
}

func printWorkflowGymSummary(stdout io.Writer, registry workflowgym.Registry) {
	pf(stdout, "datasetVersion: %s\n", registry.DatasetVersion)
	pf(stdout, "datasetDigest: %s\n", registry.Digest)
	pf(stdout, "taskCount: %d\n", len(registry.Tasks))
}
