package main

import (
	"errors"
	"flag"
	"io"
	"os"
	"sort"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/workflow"
)

func runWorkflow(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		workflowUsage(stderr)
		return 2
	}
	switch args[0] {
	case "show":
		return runWorkflowShow(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		workflowUsage(stdout)
		return 0
	default:
		pf(stderr, "goobers workflow: unknown subcommand %q\n\n", args[0])
		workflowUsage(stderr)
		return 2
	}
}

func workflowUsage(w io.Writer) {
	pf(w, "Usage: goobers workflow show [--dot] <name> [path]\n\n"+
		"Show the named workflow as a text DAG or Graphviz DOT (default path \".\").\n")
}

func runWorkflowShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("workflow show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		pf(stderr, "Usage: goobers workflow show [--dot] <name> [path]\n\n"+
			"Load the named workflow from the instance config and show its stages,\n"+
			"kinds, and transition targets as a text DAG or Graphviz DOT\n"+
			"(default path \".\").\n")
	}
	dot := fs.Bool("dot", false, "emit Graphviz DOT")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fs.Usage()
		return 2
	}

	name := fs.Arg(0)
	root := "."
	if fs.NArg() == 2 {
		root = fs.Arg(1)
	}

	l := instance.NewLayout(root)
	if _, err := os.Stat(l.ConfigFile()); err != nil {
		pf(stderr, "error: %s not found (not an instance root; run `goobers init` first)\n", l.ConfigFile())
		return 2
	}

	set, _, err := instance.LoadConfigDir(l.ConfigDir())
	if err != nil {
		pf(stderr, "error: %v\n", err)
		if errors.Is(err, instance.ErrInvalidConfig) {
			return 1
		}
		return 2
	}
	for _, wf := range set.Workflows {
		if wf.Name == name {
			if *dot {
				machine, err := workflow.Compile(workflow.Definition{
					Name: wf.Name, Version: 1, Spec: wf.Spec,
				})
				if err != nil {
					pf(stderr, "error: compile workflow %q: %v\n", wf.Name, err)
					return 1
				}
				printWorkflowDOT(stdout, machine.Graph())
			} else {
				printWorkflowDAG(stdout, wf)
			}
			return 0
		}
	}

	pf(stderr, "error: no workflow named %q in %s\n", name, l.ConfigDir())
	return 1
}

func printWorkflowDAG(w io.Writer, wf apiv1.Workflow) {
	pf(w, "workflow: %s\n", wf.Name)
	if len(wf.Spec.Triggers) == 1 && wf.Spec.Triggers[0].Type == apiv1.TriggerManual {
		pf(w, "triggers: manual-only\n")
	}
	pf(w, "start: %s\nstages:\n", wf.Spec.Start)
	for _, task := range wf.Spec.Tasks {
		pf(w, "  %s (kind: %s) -> %s\n", task.Name, task.Type, displayWorkflowTarget(task.Next))
	}
	for _, gate := range wf.Spec.Gates {
		pf(w, "  %s (kind: gate, evaluator: %s)\n", gate.Name, gate.Evaluator)
		for _, outcome := range orderedGateOutcomes(gate.Branches) {
			pf(w, "    %s target: %s\n", outcome, displayWorkflowTarget(gate.Branches[outcome]))
		}
	}
}

func printWorkflowDOT(w io.Writer, graph workflow.Graph) {
	pf(w, "digraph {\n")
	edge := 0
	for _, node := range graph.Nodes {
		shape := "box"
		if node.Kind == workflow.GraphNodeGate {
			shape = "diamond"
		}
		pf(w, "  %q [shape=%s];\n", node.ID, shape)
		for edge < len(graph.Edges) && graph.Edges[edge].Source == node.ID {
			transition := graph.Edges[edge]
			if node.Kind == workflow.GraphNodeGate {
				pf(w, "  %q -> %q [label=%q];\n",
					transition.Source, displayWorkflowTarget(transition.Target), transition.Outcome)
			} else {
				pf(w, "  %q -> %q;\n",
					transition.Source, displayWorkflowTarget(transition.Target))
			}
			edge++
		}
	}
	pf(w, "}\n")
}

func orderedGateOutcomes(branches map[string]string) []string {
	outcomes := make([]string, 0, len(branches))
	for _, outcome := range []string{"pass", "fail"} {
		if _, ok := branches[outcome]; ok {
			outcomes = append(outcomes, outcome)
		}
	}

	var remaining []string
	for outcome := range branches {
		if outcome != "pass" && outcome != "fail" {
			remaining = append(remaining, outcome)
		}
	}
	sort.Strings(remaining)
	return append(outcomes, remaining...)
}

func displayWorkflowTarget(target string) string {
	if target == "" {
		return "<complete>"
	}
	return target
}
