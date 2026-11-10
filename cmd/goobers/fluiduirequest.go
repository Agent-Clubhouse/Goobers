package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/fluidui"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
)

const fluidUIRequestHelp = "Usage: goobers fluid-ui-request (--load | --validate) [path]\n\n" +
	"fluid-ui-request is the fluid-ui-customize workflow's deterministic stage\n" +
	"(Labs Fluid UI, docs/labs-fluid-ui.md).\n\n" +
	"--load writes the ID of the request this run serves (never its text), the\n" +
	"requester's current portal.customization/v1 document and the surface to\n" +
	"the result file, for the propose stage to read. The daemon binds the run to\n" +
	"its request when the trigger is accepted; --load waits briefly for that\n" +
	"binding.\n\n" +
	"--validate reads the propose stage's candidate and classifies it as a\n" +
	"valid customization, an explicit unsupported request, or invalid output\n" +
	"(repassable once, with its validation errors). It never applies anything:\n" +
	"the daemon re-validates the candidate and the user applies it.\n\n" +
	"Exit codes: 0 = result written (an invalid candidate is not an error) /\n" +
	"1 = IO or binding error / 2 = usage error.\n"

const (
	fluidUICandidateFile = "fluid-ui-candidate.json"
	fluidUIBindingWait   = 30 * time.Second
)

func runFluidUIRequest(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("fluid-ui-request", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "fluid-ui-request")
	load := fs.Bool("load", false, "write the bound request and current customization")
	validate := fs.Bool("validate", false, "validate the propose stage's candidate")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root, ok := providerStageRootArg(fs)
	if !ok || *load == *validate {
		if ok {
			fs.Usage()
		}
		return 2
	}
	if *load {
		return runFluidUILoad(root, stdout, stderr)
	}
	return runFluidUIValidate(root, stdout, stderr)
}

func runFluidUILoad(root string, stdout, stderr io.Writer) int {
	runID := os.Getenv(executor.RunIDEnvVar)
	requests := fluidui.NewRequests(fluidui.ForInstance(root), nil)
	deadline := time.Now().Add(fluidUIBindingWait)
	input, err := requests.InputForRun(runID)
	for errors.Is(err, fluidui.ErrBindingNotFound) && time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
		input, err = requests.InputForRun(runID)
	}
	if err != nil {
		pf(stderr, "error: load fluid UI request for run %q: %v\n", runID, err)
		return 1
	}
	if err := writeFluidUIResult(providerInput("resultFile", "fluid-ui-request.json"), input); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pf(stdout, "loaded fluid UI request %s (attempt %d)\n", input.Request.ID, input.Attempt)
	return 0
}

func runFluidUIValidate(root string, stdout, stderr io.Writer) int {
	data, err := readFluidUICandidate(root)
	if err != nil {
		pf(stderr, "error: read candidate: %v\n", err)
		return 1
	}
	result := fluidui.CandidateResult{Outcome: fluidui.OutcomeInvalid, Errors: []string{"propose wrote no " + fluidUICandidateFile}, ErrorSummary: "propose wrote no " + fluidUICandidateFile, Repassable: true}
	if data != nil {
		result = fluidui.EvaluateCandidate(data)
	}
	if err := writeFluidUIResult(providerInput("resultFile", "fluid-ui-validation.json"), result); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pf(stdout, "fluid UI candidate outcome: %s\n", result.Outcome)
	return 0
}

// readFluidUICandidate reads the candidate from the workspace when present,
// otherwise from the propose stage's newest artifact in this run's journal.
// A nil result means propose wrote no candidate at all.
func readFluidUICandidate(root string) ([]byte, error) {
	path := providerInput("candidateFile", fluidUICandidateFile)
	data, err := os.ReadFile(path)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return data, err
	}
	runID := os.Getenv(executor.RunIDEnvVar)
	if runID == "" {
		return nil, nil
	}
	reader, err := stageRunJournal(root, runID)
	if err != nil {
		return nil, fmt.Errorf("open run journal: %w", err)
	}
	events, err := reader.Events()
	if err != nil {
		return nil, fmt.Errorf("read run journal: %w", err)
	}
	ref, ok := decompositionStageArtifact(events, "propose", "/"+fluidUICandidateFile)
	if !ok {
		return nil, nil
	}
	return reader.ArtifactBytes(ref)
}

// labsFluidUIOptions wires the Labs Fluid UI store and its request intake;
// request status is read from the run journals under layout.
func labsFluidUIOptions(layout instance.Layout) []httpapi.HandlerOption {
	store := fluidui.ForInstance(layout.Root)
	return []httpapi.HandlerOption{
		httpapi.WithLabsFluidUI(store),
		httpapi.WithLabsFluidUIRequests(fluidui.NewRequests(store, fluidui.JournalRuns(layout))),
	}
}

func writeFluidUIResult(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
