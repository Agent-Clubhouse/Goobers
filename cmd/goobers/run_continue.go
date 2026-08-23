package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/workflow"
)

const runContinueHelp = "Usage: goobers run continue --from <run-id> --terminal-seq <seq> --target <state> --operator <id> [path]\n\n" +
	"Create a distinct continuation journal from a terminal run. The source\n" +
	"journal is never modified and no workflow stages are executed.\n"

func runRunContinue(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("run continue", flag.ContinueOnError)
	fs.SetOutput(stderr)
	from := fs.String("from", "", "terminal source run id")
	terminalSeq := fs.Uint64("terminal-seq", 0, "source run.finished sequence")
	target := fs.String("target", "", "requested workflow state")
	operator := fs.String("operator", "", "operator identity")
	integrity := fs.String("integrity", string(apiv1.IntegrityUnapproved), "integrity grade for injected inputs")
	var inputFlags stringListFlag
	fs.Var(&inputFlags, "input", "injected input as name=path (repeatable)")
	fs.Usage = func() { pf(stderr, "%s", runContinueHelp) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *from == "" || *terminalSeq == 0 || *target == "" || *operator == "" ||
		fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}
	sourceDir, err := instance.NewLayout(root).FindRunDir(*from)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	sourceID := filepath.Base(sourceDir)
	sourceReader, err := journal.OpenReadOnly(sourceDir)
	if err != nil {
		pf(stderr, "error: inspect continuation source: %v\n", err)
		return 1
	}
	sourceIdentity, err := sourceReader.Identity()
	if err != nil {
		pf(stderr, "error: read continuation source identity: %v\n", err)
		return 1
	}
	if sourceIdentity.WorkflowDigest != "" {
		pinnedMachine, err := runner.PinnedWorkflowMachine(sourceReader, sourceIdentity)
		if err != nil {
			pf(stderr, "error: resolve continuation source workflow: %v\n", err)
			return 1
		}
		candidateMachine, err := currentWorkflowMachine(root, sourceIdentity)
		if err != nil {
			pf(stderr, "error: resolve continuation candidate workflow: %v\n", err)
			return 1
		}
		if err := runner.ValidateContinuationTarget(pinnedMachine, candidateMachine, *target); err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
	}
	inputs := make(map[string][]byte, len(inputFlags))
	inputSources := make(map[string]string, len(inputFlags))
	for _, value := range inputFlags {
		name, path, ok := strings.Cut(value, "=")
		if !ok || name == "" || path == "" {
			pf(stderr, "error: invalid --input %q; expected name=path\n", value)
			return 2
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			pf(stderr, "error: read injected input %q: %v\n", name, readErr)
			return 2
		}
		inputs[name] = data
		inputSources[name] = path
	}
	grade := apiv1.Integrity(*integrity)
	if !grade.Valid() {
		pf(stderr, "error: invalid input integrity %q\n", *integrity)
		return 2
	}
	runID, err := telemetry.NewRunID()
	if err != nil {
		pf(stderr, "error: create continuation run id: %v\n", err)
		return 2
	}
	continuation, err := journal.CreateContinuation(
		filepath.Dir(sourceDir),
		journal.ContinuationRequest{
			RunID: runID, SourceRunID: sourceID, ExpectedTerminalSeq: *terminalSeq,
			Operator: *operator, Target: *target, Inputs: inputs,
			InputIntegrity: inputIntegrityMap(inputFlags, grade),
			InputSource:    inputSources,
		},
	)
	if err != nil {
		pf(stderr, "error: create continuation: %v\n", err)
		return 1
	}
	if err := continuation.Close(); err != nil {
		pf(stderr, "error: close continuation journal: %v\n", err)
		return 2
	}
	pf(stdout, "%s\n", runID)
	return 0
}

func currentWorkflowMachine(root string, source journal.RunIdentity) (*workflow.Machine, error) {
	set, _, err := instance.LoadConfigDir(instance.NewLayout(root).ConfigDir())
	if err != nil {
		return nil, err
	}
	for _, definition := range set.Workflows {
		if definition.Name != source.Workflow || definition.Spec.Gaggle != source.Gaggle {
			continue
		}
		return workflow.Compile(workflow.Definition{
			Name: definition.Name, Version: source.WorkflowVersion,
			DSLVersion: definition.DSLVersion, Spec: definition.Spec,
		}, workflow.WithPreviewFeatures(
			set.Manifest != nil && workflow.PreviewFeaturesEnabled(set.Manifest.Annotations),
		))
	}
	return nil, fmt.Errorf("workflow %q for gaggle %q is not configured", source.Workflow, source.Gaggle)
}

type stringListFlag []string

func (f *stringListFlag) String() string { return strings.Join(*f, ",") }
func (f *stringListFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func inputIntegrityMap(inputs stringListFlag, grade apiv1.Integrity) map[string]apiv1.Integrity {
	result := make(map[string]apiv1.Integrity, len(inputs))
	for _, value := range inputs {
		name, _, _ := strings.Cut(value, "=")
		result[name] = grade
	}
	return result
}
