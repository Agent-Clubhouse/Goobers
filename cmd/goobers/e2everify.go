package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/e2e"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/runnersolve"
	"github.com/goobers/goobers/internal/version"
)

// errE2EGaggleMismatch marks a --gaggle check failure: the wrong run was
// named, exit 1 (mirroring resolveRunID's not-found case) rather than the
// generic exit 2 every other e2eBuildBundle error takes.
var errE2EGaggleMismatch = errors.New("run belongs to a different gaggle")

// itemArch11Item7LedgerWindows is goobernetes-architecture.md §11 item 7's id
// (internal/e2e.AssertNoLedgerTouchingOnWindows), following the same
// "ARCH11-<item>" spelling internal/e2e.ItemArch11Item8CapabilityGap already
// uses for item 8. internal/e2e carries no constant for item 7 — this
// command's own value fills that gap without adding an unused export to the
// harness package (this command is item 7's only caller today).
const itemArch11Item7LedgerWindows e2e.SmokeItemID = "ARCH11-7"

// e2eJournalLiveVisibilityObserver names S8's RECORDED-DATA proxy this
// command checks, distinct from internal/e2e.LiveVisibilityObserver (a live
// portal/SSE capture, which this command never takes — it verifies an
// already-completed run, after the fact). The proxy is strictly weaker: it
// proves the run's stage.started/stage.finished journal events are
// genuinely incremental (timestamped before the run's terminal event, not
// backfilled), which S8's real observer needs but does not by itself prove
// the portal RENDERED a transition while the run was still in flight. A
// consumer that captured a true SSE/portal observation should treat that as
// the authoritative S8 evidence; this command's S8 item is a floor, not a
// substitute.
const e2eJournalLiveVisibilityObserver = "run's stage.started/stage.finished journal events (internal/readservice RunEvent.Time) vs. the run's terminal event time — a recorded-data proxy for internal/e2e.LiveVisibilityObserver's live portal/SSE capture; see this command's -h for the distinction"

const e2eHelp = "Usage: goobers e2e verify [flags]\n\n" +
	"verify: check the Goobernetes S1-S9 distributed e2e proof harness's\n" +
	"        assertions (#3517) against one already-completed run's recorded\n" +
	"        data.\n"

func runE2E(args []string, stdout, stderr io.Writer) int {
	usage := func(w io.Writer) { pf(w, "%s", e2eHelp) }
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	default:
		pf(stderr, "goobers e2e: unknown subcommand %q\n\n", args[0])
		usage(stderr)
		return 2
	}
}

const e2eVerifyHelp = `Usage: goobers e2e verify --run <run-id> [--gaggle <name>] [--expected <topology.json>] [--out <bundle.json>] [path]

Verify the Goobernetes S1-S9 distributed e2e proof harness's assertions
(#3517, docs/design/goobernetes-smoke.md) against one already-completed
run's recorded data — StageAttempt placement provenance and the run
journal, fetched the same way ` + "`goobers trace`" + ` and ` + "`goobers runs list`" + ` do. This
command drives no cluster, applies no workflow, and kills nothing. It
verifies a run that already happened; a separate, infra-side orchestration
(outside this repo, likely shell) produces the run and calls this command
afterward.

Always checked, from the run's recorded data alone:
  S1        fresh pod per stage attempt, never reused
  S2        at least one Linux and one Windows stage attempt, run completed
  S8        stage-transition journal events precede the run's terminal event
            — a RECORDED-DATA PROXY for S8's real observer (a live
            portal/SSE capture): it proves the journal trail is genuinely
            incremental, not that the portal rendered a transition live.

Checked only when --expected supplies the needed data; skipped (not
recorded as a failure) otherwise:
  ARCH11-7  no ledger-touching stage attempt placed on Windows (needs
            "ledgerTouchingStages")
  ARCH11-8  a declared capability gap was caught (needs "capabilityGap")
  S9        the network:allowlist negative-control triple (needs
            "negativeControl")

--expected is a JSON file supplying deployment-specific expectations this
command hardcodes none of:

  {
    "ledgerTouchingStages": ["implement"],
    "capabilityGap": {
      "wantUnsatStage": "windows-only-stage",
      "unsatisfiableStages": [
        {"stage": "windows-only-stage", "kind": "requirement", "diagnostic": "..."}
      ]
    },
    "negativeControl": {
      "denial":          {"endpoint": "blocked.example.com:443", "exitCode": 28},
      "positiveControl": {"endpoint": "allowed.example.com:443", "exitCode": 0},
      "modelEndpoints":  ["api.anthropic.com"],
      "controlVantage":  {"endpoint": "blocked.example.com:443", "exitCode": 0}
    }
  }

S3-S7 (declared-edge handoff, artifact materialization, repass, write-API
trigger/escalation, and the S6 kill matrix) need a live topology-driving
orchestration to produce their evidence and are not checked by this
command — see internal/e2e's per-item doc comments.

--out writes the evidence bundle (internal/e2e.Bundle, JSON) there; default
is stdout. Exit codes: 0 = every checked item passed, 1 = at least one
checked item failed or was invalid / business error, 2 = usage/IO error.
`

// e2eExpectedTopology is the --expected file's schema: the deployment-
// specific facts this command needs but refuses to hardcode (per the #3517
// task: "the command hardcodes NO topology"). Every field is optional —
// absence means the corresponding item is skipped, not failed, since a
// skipped item was never asked (mirrors internal/e2e.Bundle.MissingItems'
// own "never asked" distinction from "asked and failed").
type e2eExpectedTopology struct {
	// LedgerTouchingStages names the compiled workflow's ledger-touching
	// stages (internal/bootstrap/placement.go taskLedgerTouching derives the
	// same set from the DSL; this command consumes the CALLER's view of it
	// rather than recompiling the workflow, per internal/e2e/ledgerwindows.go's
	// LedgerTouching doc comment).
	LedgerTouchingStages []string `json:"ledgerTouchingStages,omitempty"`
	// CapabilityGap supplies architecture §11 item 8's evidence: the
	// consumer's own capture of a runnersolve.Solve/SolveExecutable result
	// against a config with a deliberately unsatisfiable stage (e.g. from
	// `goobers validate --json` or a daemon boot refusal), because that solve
	// is a workflow/daemon-admission fact, not part of any one run's
	// StageAttempt/journal data this command otherwise reads.
	CapabilityGap *e2eCapabilityGapExpectation `json:"capabilityGap,omitempty"`
	// NegativeControl supplies S9's three-leg curl probe triple — an
	// out-of-band network probe no run journal records.
	NegativeControl *e2eNegativeControlExpectation `json:"negativeControl,omitempty"`
}

type e2eCapabilityGapExpectation struct {
	WantUnsatStage      string                     `json:"wantUnsatStage"`
	UnsatisfiableStages []e2eUnsatStageExpectation `json:"unsatisfiableStages"`
}

type e2eUnsatStageExpectation struct {
	Stage      string `json:"stage"`
	Kind       string `json:"kind"`
	Diagnostic string `json:"diagnostic"`
}

type e2eNegativeControlExpectation struct {
	Denial          e2eProbeExpectation `json:"denial"`
	PositiveControl e2eProbeExpectation `json:"positiveControl"`
	ModelEndpoints  []string            `json:"modelEndpoints,omitempty"`
	ControlVantage  e2eProbeExpectation `json:"controlVantage"`
}

type e2eProbeExpectation struct {
	Endpoint string `json:"endpoint"`
	ExitCode int    `json:"exitCode"`
}

func runE2EVerify(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("e2e verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	runIDFlag := fs.String("run", "", "run id to verify (required)")
	gaggle := fs.String("gaggle", "", "require the run belong to this gaggle")
	expectedPath := fs.String("expected", "", "JSON file supplying deployment-specific topology expectations")
	outPath := fs.String("out", "", "write the evidence bundle here instead of stdout")
	fs.Usage = helpUsage(stderr, "e2e verify")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*runIDFlag) == "" {
		pf(stderr, "error: --run is required\n\n")
		fs.Usage()
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}

	var expected e2eExpectedTopology
	if *expectedPath != "" {
		data, err := os.ReadFile(*expectedPath)
		if err != nil {
			pf(stderr, "error: read --expected: %v\n", err)
			return 2
		}
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&expected); err != nil {
			pf(stderr, "error: parse --expected %s: %v\n", *expectedPath, err)
			return 2
		}
	}

	l := instance.NewLayout(root)
	resolvedRunID, err := resolveRunID(l, *runIDFlag)
	if errors.Is(err, iofs.ErrNotExist) {
		pf(stderr, "error: no run %q found in %s; list runs with 'goobers status'\n", *runIDFlag, root)
		return 1
	}
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	reads, err := readservice.NewOfflineRuns(l)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	bundle, coverage, err := e2eBuildBundle(context.Background(), reads, l, resolvedRunID, *gaggle, expected, time.Now)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		if errors.Is(err, errE2EGaggleMismatch) {
			return 1
		}
		return 2
	}

	out := stdout
	var outFile *os.File
	if *outPath != "" {
		outFile, err = os.Create(*outPath)
		if err != nil {
			pf(stderr, "error: create --out %s: %v\n", *outPath, err)
			return 2
		}
		out = outFile
	}
	encodeErr := bundle.Encode(out)
	if outFile != nil {
		closeErr := outFile.Close()
		if encodeErr == nil {
			encodeErr = closeErr
		}
	}
	if encodeErr != nil {
		pf(stderr, "error: write evidence bundle: %v\n", encodeErr)
		return 2
	}

	pf(stderr, "checked: %s\n", strings.Join(coverage, ", "))
	if skipped := e2eSkippedItems(bundle, expected); len(skipped) > 0 {
		pf(stderr, "skipped (no --expected data, or needs a live topology-driving orchestration this command does not do): %s\n", strings.Join(skipped, ", "))
	}

	if bundle.Overall() != e2e.VerdictPass {
		return 1
	}
	return 0
}

// e2eSkippedItems reports every item this run of the command did not
// record a verdict for: S3-S7 (internal/e2e.RequiredSmokeItems' S1-S9 set,
// via Bundle.MissingItems — these always need a live topology-driving
// orchestration this command explicitly does not do) plus ARCH11-7/ARCH11-8
// when --expected did not supply their data. Never a failure by itself — a
// skipped item was never asked, which internal/e2e.Bundle.MissingItems'
// own doc comment distinguishes from asked-and-failed.
func e2eSkippedItems(bundle e2e.Bundle, expected e2eExpectedTopology) []string {
	skipped := make([]string, 0, 8)
	for _, item := range bundle.MissingItems(e2e.RequiredSmokeItems()) {
		skipped = append(skipped, string(item))
	}
	if len(expected.LedgerTouchingStages) == 0 {
		skipped = append(skipped, string(itemArch11Item7LedgerWindows))
	}
	if expected.CapabilityGap == nil {
		skipped = append(skipped, string(e2e.ItemArch11Item8CapabilityGap))
	}
	return skipped
}

// e2eBuildBundle fetches runID's recorded data through reads (the same
// read-side `goobers trace` uses) and runs every internal/e2e assertion
// helper this command wires up, returning the resulting evidence Bundle and
// the ids it actually recorded (for the human-readable coverage line). now
// is injected for deterministic tests.
func e2eBuildBundle(
	ctx context.Context,
	reads readservice.OfflineRuns,
	layout instance.Layout,
	runID, wantGaggle string,
	expected e2eExpectedTopology,
	now func() time.Time,
) (e2e.Bundle, []string, error) {
	detail, err := reads.GetRun(ctx, runID)
	if err != nil {
		return e2e.Bundle{}, nil, fmt.Errorf("get run %q: %w", runID, err)
	}
	if wantGaggle != "" && detail.Gaggle != wantGaggle {
		return e2e.Bundle{}, nil, fmt.Errorf("run %q belongs to gaggle %q, not %q: %w", runID, detail.Gaggle, wantGaggle, errE2EGaggleMismatch)
	}

	stages := make([]readservice.AttemptList, 0, len(detail.Stages))
	for _, stageName := range detail.Stages {
		list, err := reads.StageAttempts(ctx, runID, stageName)
		if err != nil {
			return e2e.Bundle{}, nil, fmt.Errorf("stage attempts for %q: %w", stageName, err)
		}
		stages = append(stages, list)
	}

	events, err := reads.RunEvents(ctx, runID)
	if err != nil {
		return e2e.Bundle{}, nil, fmt.Errorf("run events: %w", err)
	}

	recordedAt := now().UTC()
	info := version.Get()
	bundle := e2e.Bundle{
		ProcedureID: fmt.Sprintf("e2e-verify:%s", runID),
		StartedAt:   recordedAt,
		Collateral: e2e.Collateral{
			BinaryVersion: info.Version,
			CommitSHA:     info.Commit,
		},
	}
	if runDir, err := runDirFor(layout, runID); err == nil {
		bundle.Collateral.RunJournalPaths = []string{runDir}
	}

	var coverage []string
	add := func(item e2e.SmokeItemID, observer string, result e2e.AssertionResult) error {
		obsResult, err := e2e.NewObserverResult(item, observer, result, recordedAt)
		if err != nil {
			return fmt.Errorf("record %s: %w", item, err)
		}
		bundle.Add(obsResult)
		coverage = append(coverage, string(item))
		return nil
	}

	// S1: fresh pod per stage attempt (always checkable from recorded data).
	if err := add(e2e.ItemS1FreshPod, e2e.FreshPodObserver, e2e.AssertFreshPodPerAttempt(stages)); err != nil {
		return e2e.Bundle{}, nil, err
	}

	// S2: at least one Linux and one Windows attempt, run completed (always
	// checkable from recorded data).
	runCompleted := detail.Phase == journal.PhaseCompleted
	if err := add(e2e.ItemS2OSHop, e2e.OSHopObserver, e2e.AssertOSHop(stages, runCompleted)); err != nil {
		return e2e.Bundle{}, nil, err
	}

	// S8: journal-derived live-visibility proxy (always attempted; INVALID
	// when the run carries no terminal event yet).
	var terminalAt time.Time
	if detail.FinishedAt != nil {
		terminalAt = *detail.FinishedAt
	}
	observations := e2eJournalStageTransitionObservations(events.Events)
	liveVis := e2e.AssertLiveVisibility(observations, terminalAt)
	if err := add(e2e.ItemS8LiveVisibility, e2eJournalLiveVisibilityObserver, liveVis); err != nil {
		return e2e.Bundle{}, nil, err
	}

	// ARCH11-7: no ledger-touching stage attempt on Windows (only when
	// --expected names the ledger-touching stages).
	if len(expected.LedgerTouchingStages) > 0 {
		ledgerSet := make(map[string]bool, len(expected.LedgerTouchingStages))
		for _, s := range expected.LedgerTouchingStages {
			ledgerSet[s] = true
		}
		ledgerTouching := e2e.LedgerTouching(func(stage string) bool { return ledgerSet[stage] })
		result := e2e.AssertNoLedgerTouchingOnWindows(stages, ledgerTouching)
		if err := add(itemArch11Item7LedgerWindows, e2e.LedgerNeverOnWindowsObserver, result); err != nil {
			return e2e.Bundle{}, nil, err
		}
	}

	// ARCH11-8: capability gap enforced (only when --expected supplies the
	// solve evidence — see e2eCapabilityGapExpectation's doc comment for why
	// this is not derived from the run's own recorded data).
	if expected.CapabilityGap != nil {
		var solveResult runnersolve.Result
		for _, s := range expected.CapabilityGap.UnsatisfiableStages {
			solveResult.Stages = append(solveResult.Stages, runnersolve.StagePlacement{
				Stage: s.Stage,
				Unsat: &runnersolve.Unsat{Kind: runnersolve.UnsatKind(s.Kind), Diagnostic: s.Diagnostic},
			})
		}
		result := e2e.AssertCapabilityGapEnforced(solveResult, expected.CapabilityGap.WantUnsatStage)
		if err := add(e2e.ItemArch11Item8CapabilityGap, e2e.CapabilityGapObserver, result); err != nil {
			return e2e.Bundle{}, nil, err
		}
	}

	// S9: the network:allowlist negative-control triple (only when
	// --expected supplies the out-of-band probe evidence).
	if expected.NegativeControl != nil {
		nc := expected.NegativeControl
		triple := e2e.NegativeControlTriple{
			Denial:          e2e.NegativeControlProbe{Endpoint: nc.Denial.Endpoint, ExitCode: nc.Denial.ExitCode},
			PositiveControl: e2e.NegativeControlProbe{Endpoint: nc.PositiveControl.Endpoint, ExitCode: nc.PositiveControl.ExitCode},
			ModelEndpoints:  nc.ModelEndpoints,
			ControlVantage:  e2e.NegativeControlProbe{Endpoint: nc.ControlVantage.Endpoint, ExitCode: nc.ControlVantage.ExitCode},
		}
		result := e2e.AssertNegativeControlTriple(triple)
		if err := add(e2e.ItemS9NegativeControl, e2e.NegativeControlObserver, result); err != nil {
			return e2e.Bundle{}, nil, err
		}
	}

	bundle.FinishedAt = now().UTC()
	return bundle, coverage, nil
}

// e2eJournalStageTransitionObservations projects a run's stage.started/
// stage.finished events into internal/e2e.StageTransitionObservation values
// — S8's journal-derived proxy observation (see e2eJournalLiveVisibilityObserver's
// doc comment). Source is "journal" throughout so a bundle reader can never
// mistake these for a true portal/SSE capture (Source "sse"/"portal-screenshot").
func e2eJournalStageTransitionObservations(events []readservice.RunEvent) []e2e.StageTransitionObservation {
	var out []e2e.StageTransitionObservation
	for _, event := range events {
		if event.Stage == "" || event.Time.IsZero() {
			continue
		}
		switch event.Type {
		case journal.EventStageStarted, journal.EventStageFinished:
			out = append(out, e2e.StageTransitionObservation{
				Source:     "journal",
				Stage:      event.Stage,
				ObservedAt: event.Time,
			})
		}
	}
	return out
}
