package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

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
	case "-h", "--help", "help":
		workflowUsage(stdout)
		return 0
	default:
		pf(stderr, "goobers workflow: unknown subcommand %q\n\n", args[0])
		workflowUsage(stderr)
		return 2
	}
}

const workflowHelp = "Usage: goobers workflow show [flags] <name> [path]\n\n" +
	"Show the named workflow as a text DAG or Graphviz DOT (default path \".\").\n\n" +
	"Workflow\n\n" +
	workflowConcept + "\n"

func workflowUsage(w io.Writer) {
	pf(w, "%s", workflowHelp)
}

const workflowShowHelp = "Usage: goobers workflow show [--dot] <name> [path]\n\n" +
	"Load the named workflow from the instance config and show its stages,\n" +
	"kinds, and transition targets as a text DAG or Graphviz DOT\n" +
	"(default path \".\").\n"

func runWorkflowShow(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("workflow show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "workflow show")
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

	l := layoutFor(root)
	if _, err := os.Stat(l.ConfigFile()); err != nil {
		pf(stderr, "error: %s not found (not an instance root; run `goobers init` first)\n", l.ConfigFile())
		return 2
	}

	set, report, err := instance.LoadConfigDir(l.ConfigDir())
	if err != nil {
		printValidationIssues(stderr, report)
		pf(stderr, "error: %v\n", err)
		if errors.Is(err, instance.ErrInvalidConfig) {
			return 1
		}
		return 2
	}
	printValidationWarnings(stderr, report.CLIWarnings())
	for _, wf := range set.Workflows {
		if wf.Name != name {
			continue
		}
		// Both projections render the compiled machine, so the text view
		// agrees with --dot and neither prints a DAG that `goobers validate`
		// rejects (#2738). Preview authorization is per-Workflow (#4220):
		// wf's OWN annotations, never the Manifest's or its gaggle's.
		machine, err := workflow.Compile(workflow.Definition{
			Name: wf.Name, Version: 1, DSLVersion: wf.DSLVersion, Spec: wf.Spec, Annotations: wf.Annotations,
		}, workflow.WithPreviewFeatures(
			workflow.PreviewFeaturesEnabled(wf.Annotations),
		))
		if err != nil {
			pf(stderr, "error: compile workflow %q: %v\n", wf.Name, err)
			return 1
		}
		if *dot {
			printWorkflowDOT(stdout, machine.Graph())
		} else {
			printWorkflowDAG(stdout, wf, machine.Graph())
		}
		return 0
	}

	pf(stderr, "error: no workflow named %q in %s\n", name, l.ConfigDir())
	return 1
}

// printWorkflowDAG renders the compiled graph as text. Nodes and edges are
// the same projection --dot draws, so a parallel is listed with its fan-out
// and failure route, and a branch's "@join" shows as its concrete join stage
// rather than a dead end.
func printWorkflowDAG(w io.Writer, wf apiv1.Workflow, graph workflow.Graph) {
	pf(w, "workflow: %s\n", wf.Name)
	if len(wf.Spec.Triggers) == 1 && wf.Spec.Triggers[0].Type == apiv1.TriggerManual {
		pf(w, "triggers: manual-only\n")
	}
	pf(w, "start: %s\nstages:\n", graph.Start)
	outgoing := make(map[string][]workflow.GraphEdge, len(graph.Nodes))
	for _, e := range graph.Edges {
		outgoing[e.Source] = append(outgoing[e.Source], e)
	}
	for _, node := range graph.Nodes {
		out := outgoing[node.ID]
		switch node.Kind {
		case workflow.GraphNodeGate:
			pf(w, "  %s (kind: gate, evaluator: %s)\n", node.ID, node.Evaluator)
		case workflow.GraphNodeParallel:
			pf(w, "  %s (kind: parallel, %s)\n", node.ID, parallelConcurrency(wf.Spec.Parallels, node.ID))
		default:
			for _, e := range out {
				pf(w, "  %s (kind: %s) -> %s\n", node.ID, node.Kind, displayWorkflowTarget(e.Target))
			}
			continue
		}
		for _, e := range out {
			if e.Branch != "" {
				pf(w, "    branch %s start: %s\n", e.Branch, e.Target)
				continue
			}
			pf(w, "    %s target: %s\n", e.Outcome, displayWorkflowTarget(e.Target))
		}
	}
}

// parallelConcurrency describes how many of a parallel's branches run at
// once. Unset maxConcurrentBranches means 1: the branches run sequentially.
func parallelConcurrency(parallels []apiv1.Parallel, name string) string {
	for _, p := range parallels {
		if p.Name != name {
			continue
		}
		switch p.MaxConcurrentBranches {
		case 0:
			return "maxConcurrentBranches unset: branches run sequentially"
		case 1:
			return "maxConcurrentBranches: 1, branches run sequentially"
		default:
			return fmt.Sprintf("maxConcurrentBranches: %d", p.MaxConcurrentBranches)
		}
	}
	return "maxConcurrentBranches: unknown"
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

func displayWorkflowTarget(target string) string {
	if target == "" {
		return "<complete>"
	}
	return target
}
