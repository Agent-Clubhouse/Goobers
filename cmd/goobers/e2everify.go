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
	"github.com/goobers/goobers/internal/runnercap"
	"github.com/goobers/goobers/internal/runnersolve"
	"github.com/goobers/goobers/internal/version"
)

// itemArch11Item7LedgerWindows is goobernetes-architecture.md §11 item 7's id
// (internal/e2e.AssertNoLedgerTouchingOnWindows), following the same
// "ARCH11-<item>" spelling internal/e2e.ItemArch11Item8CapabilityGap already
// uses for item 8. internal/e2e carries no constant for item 7 — this
// command's own value fills that gap without adding an unused export to the
// harness package (this command is item 7's only caller today).
const itemArch11Item7LedgerWindows e2e.SmokeItemID = "ARCH11-7"

const e2eHelp = "Usage: goobers e2e verify [flags]\n" +
	"       goobers e2e kill-inject [flags]\n\n" +
	"verify:      check the Goobernetes S1-S9 distributed e2e proof harness's\n" +
	"             assertions (#3517) against one already-completed run's\n" +
	"             recorded data.\n" +
	"kill-inject: perform one live S6 kill-matrix cell (pod-kill) against a\n" +
	"             real cluster (#3513).\n"

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
       goobers e2e verify --print-runner-class <restriction[,restriction...]>

Verify the Goobernetes S1-S9 distributed e2e proof harness's assertions
(#3517, docs/design/goobernetes-smoke.md) against one already-completed
run's recorded data — StageAttempt placement provenance and the run
journal, fetched the same way ` + "`goobers trace`" + ` and ` + "`goobers runs list`" + ` do. This
command drives no cluster, applies no workflow, and kills nothing. It
verifies a run that already happened; a separate, infra-side orchestration
(outside this repo, likely shell) produces the run and calls this command
afterward.

PURE FUNCTION, RE-RUNNABLE FROM ITSELF (goobernetes-smoke.md §5 rule 4):
every verdict is computed from the run's on-disk journal (via the read
side), --expected, and whatever captures --expected inlines — never a
cluster, network, or other live dependency. The emitted bundle records
--expected's own path (Collateral.S8CapturePath/S9ProbeOutputPath, when
those items were checked) alongside the run's journal directory
(Collateral.RunJournalPaths), so every verdict is re-derivable offline
from the bundle plus those two paths alone.

Always checked, from the run's recorded data alone:
  S1        fresh pod per stage attempt, never reused
  S2        at least one Linux and one Windows stage attempt, run completed

Checked only when --expected supplies the needed data; skipped (not
recorded as a failure) otherwise:
  S8        a live portal/SSE observation of a stage transition, timestamped
            before the run's terminal event (needs "liveVisibility"). A
            JOURNAL timestamp cannot stand in for this: a closed-run
            projection has the same early stage.started/stage.finished
            times a genuine live capture would, so it is blind to exactly
            the failure mode S8 exists to catch (terminal-only visibility)
            — per goobernetes-smoke.md §5 rule 2, an observer that was not
            actually exercised is never a pass. Supply the consumer's own
            recorded portal/SSE capture, or S8 is skipped, never passed.
  ARCH11-7  no ledger-touching stage attempt placed on Windows (needs
            "ledgerTouchingStages")
  ARCH11-8  a declared capability gap was caught (needs "capabilityGap")
  S9        the network:allowlist negative-control triple (needs
            "negativeControl")

--expected DECLARES INTENDED TOPOLOGY — what the operator meant to deploy,
or what the workflow's own DSL declares should happen — never a restatement
of the run's actual recorded data (that would be circular: checking the
run against a description of itself). A mismatch between --expected and
the recorded run is a real FINDING (an item FAILS), never a usage error:

  {
    "liveVisibility": {
      "source": "portal",
      "observations": [
        {"stage": "implement", "transition": "started", "observedAt": "2026-08-01T12:00:01Z"}
      ]
    },
    "ledgerTouchingStages": ["implement"],
    "capabilityGap": {
      "wantUnsatStage": "windows-only-stage",
      "unsatisfiableStages": [
        {"stage": "windows-only-stage", "kind": "requirement", "diagnostic": "..."}
      ]
    },
    "negativeControl": {
      "denial":                            {"endpoint": "blocked.example.com:443", "exitCode": 28},
      "positiveControl":                   {"endpoint": "allowed.example.com:443", "exitCode": 0},
      "modelEndpoints":                    ["api.anthropic.com"],
      "controlVantage":                    {"endpoint": "blocked.example.com:443", "exitCode": 0},
      "restrictedRunnerRestrictions":      ["network:allowlist"],
      "controlVantageRunnerRestrictions":  []
    }
  }

negativeControl's two "...Restrictions" fields name the RESTRICTION EFFECTS
(the same closed vocabulary a runners: inventory entry declares) of the
Denial/PositiveControl leg's restricted runner and the ControlVantage leg's
second runner, not a runner-class LABEL VALUE — this command GENERATES that
value itself via internal/runnercap.RunnerClassValue (delivery decision
015, the dispatcher's own stamping function), the same way
--print-runner-class does, so a topology file never hand-transcribes a
class string that could drift from what the dispatcher actually stamps.
Use --print-runner-class <restriction[,restriction...]> to see the derived
value before authoring a topology file; it needs no --run, no instance
root, and touches no cluster.

S3-S7 (declared-edge handoff, artifact materialization, repass, write-API
trigger/escalation, and the S6 kill matrix) need a live topology-driving
orchestration to produce their evidence and are not checked by this
command — see internal/e2e's per-item doc comments.

--out writes the evidence bundle (internal/e2e.Bundle, JSON) there; default
is stdout. Exit codes distinguish "the design/system is broken" from "the
observer machinery is broken" — opposite responses (D4/§5 rule 2's
invalid-is-never-a-pass rule applied to the driver's own control flow):
  0 = every checked item PASSED
  1 = at least one item FAILED (act on the design — this wins even if an
      INVALID item is also present; it still appears in the bundle)
  2 = usage / IO / --expected parse error / run or gaggle did not resolve
      (the command itself could not run at all — no bundle was produced)
  3 = at least one item was INVALID and none FAILED (the observer
      machinery broke, not necessarily the product — fix instrumentation
      and re-run; includes an item --expected asked this invocation to
      check but that came back unproven)
`

// e2eExpectedTopology is the --expected file's schema: the deployment-
// specific facts this command needs but refuses to hardcode (per the #3517
// task: "the command hardcodes NO topology"). Every field declares INTENDED
// topology — what the operator meant to deploy, or what the workflow's own
// DSL declares should happen — never a restatement of the run being
// verified: this command has no cluster/network access at all (see
// runE2EVerify's doc comment), so the only way --expected could describe
// "actuality" instead of "intent" would be for a caller to derive it FROM
// the same run's own recorded data before handing it back — which would
// make the check circular (comparing the run to a description of itself)
// and is exactly what this schema's fields are designed NOT to need: each
// one is either a compile-time/declared fact (LedgerTouchingStages,
// LiveVisibility/NegativeControl's runner restrictions) or an independently
// captured observation from OUTSIDE this run (LiveVisibility's live
// capture, CapabilityGap's separate admission-time solve, NegativeControl's
// out-of-band probe). A mismatch between --expected and the recorded run is
// therefore a genuine FINDING (an item FAILS), never a usage error — every
// assertion helper this command calls returns FAIL through the bundle, not
// an early exit.
//
// Every field is optional — absence means the corresponding item is
// skipped, not failed, since a skipped item was never asked (mirrors
// internal/e2e.Bundle.MissingItems' own "never asked" distinction from
// "asked and failed").
type e2eExpectedTopology struct {
	// LiveVisibility supplies S8's evidence: the consumer's own recorded
	// portal/SSE observation of a stage transition, captured WHILE THE RUN
	// WAS STILL IN FLIGHT. This can never be derived from the run's own
	// journal after the fact — a closed-run journal's stage.started/
	// stage.finished timestamps look identical whether or not anything was
	// actually watching live, so a journal-timestamp signal is blind to
	// exactly the terminal-only-visibility failure S8 exists to catch
	// (goobernetes-smoke.md §5 rule 2: an observer that was not actually
	// exercised is never a pass).
	LiveVisibility *e2eLiveVisibilityExpectation `json:"liveVisibility,omitempty"`
	// LedgerTouchingStages names the compiled workflow's ledger-touching
	// stages (internal/bootstrap/placement.go taskLedgerTouching derives the
	// same set from the DSL; this command consumes the CALLER's view of it
	// rather than recompiling the workflow, per internal/e2e/ledgerwindows.go's
	// LedgerTouching doc comment).
	LedgerTouchingStages []string `json:"ledgerTouchingStages,omitempty"`
	// CapabilityGap declares architecture §11 item 8's expected outcome: the
	// consumer's own capture of a SEPARATE runnersolve.Solve/SolveExecutable
	// admission-time solve against a config with a deliberately
	// unsatisfiable stage (e.g. from `goobers validate --json` or a daemon
	// boot refusal) — independent of the run being verified, since that
	// solve is a workflow/daemon-admission fact, not part of any one run's
	// StageAttempt/journal data this command otherwise reads.
	CapabilityGap *e2eCapabilityGapExpectation `json:"capabilityGap,omitempty"`
	// NegativeControl declares S9's three-leg curl probe triple — an
	// out-of-band network probe no run journal records, captured
	// independently of the run being verified.
	NegativeControl *e2eNegativeControlExpectation `json:"negativeControl,omitempty"`
}

type e2eLiveVisibilityExpectation struct {
	// Source names the capture mechanism ("portal" or "sse") — S8 open
	// point 3 leaves the form open; recorded per internal/e2e.
	// StageTransitionObservation's own Source field.
	Source string `json:"source"`
	// Observations is the drive's recorded sighting of each stage
	// transition, in the SAME shape internal/e2e.AssertLiveVisibility
	// consumes: this command feeds these straight through as the real S8
	// observer, never through a journal-derived substitute.
	Observations []e2eLiveVisibilityObservation `json:"observations"`
}

type e2eLiveVisibilityObservation struct {
	Stage string `json:"stage"`
	// Transition is free-text context for a bundle reader (e.g.
	// "started"/"finished", or the SSE frame's own kind) — not consumed by
	// the assertion itself, which checks only Stage/ObservedAt ordering
	// against the run's terminal event.
	Transition string `json:"transition,omitempty"`
	// ObservedAt is when the live observation was captured (RFC3339),
	// required to be strictly before the run's terminal event for at least
	// one observation.
	ObservedAt time.Time `json:"observedAt"`
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
	// RestrictedRunnerRestrictions is the restricted runner's declared
	// restriction set (goobernetes-restrictions.md §2's closed vocabulary,
	// e.g. "network:allowlist") — the SAME runner Denial and PositiveControl
	// probed from. Optional; when present this command DERIVES the runner-
	// class label value from it via internal/runnercap.RunnerClassValue
	// (decision 015, the dispatcher's one producer) and records the
	// derived value as evidence — the topology file names restrictions,
	// never a hand-transcribed class-label string, so the value can never
	// drift from what the dispatcher actually stamps.
	RestrictedRunnerRestrictions []string `json:"restrictedRunnerRestrictions,omitempty"`
	// ControlVantageRunnerRestrictions is the SECOND runner class's declared
	// restriction set (the ControlVantage leg's pod) — same generation rule
	// as RestrictedRunnerRestrictions.
	ControlVantageRunnerRestrictions []string `json:"controlVantageRunnerRestrictions,omitempty"`
}

type e2eProbeExpectation struct {
	Endpoint string `json:"endpoint"`
	ExitCode int    `json:"exitCode"`
}

// e2eNegativeControlEvidence is S9's recorded ObserverResult.Evidence: the
// real e2e.NegativeControlTriple internal/e2e.AssertNegativeControlTriple
// classified, plus the runner-class label values this command GENERATED
// from --expected's declared restriction sets (never a value the topology
// file supplied directly — see e2eNegativeControlExpectation's doc
// comment). Empty when --expected named no restrictions for that leg.
type e2eNegativeControlEvidence struct {
	Triple                    e2e.NegativeControlTriple `json:"triple"`
	RestrictedRunnerClass     string                    `json:"restrictedRunnerClass,omitempty"`
	ControlVantageRunnerClass string                    `json:"controlVantageRunnerClass,omitempty"`
}

func runE2EVerify(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("e2e verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	runIDFlag := fs.String("run", "", "run id to verify (required, unless --print-runner-class)")
	gaggle := fs.String("gaggle", "", "require the run belong to this gaggle")
	expectedPath := fs.String("expected", "", "JSON file declaring intended topology expectations")
	outPath := fs.String("out", "", "write the evidence bundle here instead of stdout")
	printRunnerClass := fs.String("print-runner-class", "", "print the runner-class label value for this comma-separated restriction set as JSON and exit; touches no run, instance, or cluster")
	fs.Usage = helpUsage(stderr, "e2e verify")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// --print-runner-class is a standalone utility, exactly like
	// netpolrender's --print-blob-endpoint: it short-circuits before any of
	// --run's requirements apply, needs no instance root, and touches no
	// cluster (internal/runnercap.RunnerClassValue is a pure function).
	printRunnerClassSelected := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "print-runner-class" {
			printRunnerClassSelected = true
		}
	})
	if printRunnerClassSelected {
		if fs.NArg() != 0 {
			fs.Usage()
			return 2
		}
		return printE2ERunnerClassJSON(stdout, stderr, *printRunnerClass)
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
		return 2
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

	// Every error e2eBuildBundle can return (run/gaggle resolution, a
	// StageAttempts read failure) means the command itself could not run —
	// no bundle was ever produced — so it is uniformly exit 2, never 1 or 3
	// (those two are reserved for what a PRODUCED bundle's items say).
	bundle, coverage, err := e2eBuildBundle(context.Background(), reads, l, resolvedRunID, *gaggle, *expectedPath, expected, time.Now)
	if err != nil {
		pf(stderr, "error: %v\n", err)
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

	return e2eExitCode(bundle)
}

// e2eExitCode implements the driver-facing exit-code contract, DISTINCT
// from Bundle.Overall()'s own precedence (Overall() lets an INVALID item
// dominate a FAIL — the right rule for "did the whole bundle prove its
// claim," per D4/§5 rule 2: an unproven claim can't be trusted even if
// something else visibly failed). A DRIVER deciding what to do NEXT needs
// the opposite precedence: a real FAIL means the design/system is broken
// and is actionable right now; an INVALID with no FAIL means the observer
// machinery broke, not (necessarily) the product — fix instrumentation and
// re-run. So FAIL wins the exit code even when an INVALID item is also
// present; that INVALID item still appears in the bundle for the re-run to
// see. Only items the bundle actually RECORDED are considered — an item
// this invocation never asked for (no --expected data, or S3-S7's
// permanently-unwired topology-pending set) never affects the exit code,
// consistent with Bundle.MissingItems' own "never asked" distinction.
func e2eExitCode(bundle e2e.Bundle) int {
	var sawFail, sawInvalid bool
	for _, item := range bundle.Items {
		switch item.Verdict {
		case e2e.VerdictFail:
			sawFail = true
		case e2e.VerdictInvalid:
			sawInvalid = true
		}
	}
	switch {
	case sawFail:
		return 1
	case sawInvalid:
		return 3
	default:
		return 0
	}
}

// printE2ERunnerClassJSON derives and prints the goobers.dev/runner-class
// label value internal/runnercap.RunnerClassValue (delivery decision 015)
// would stamp for restrictionsCSV's comma-separated restriction set — the
// SAME function the dispatcher and the NetworkPolicy renderer already
// share, so a --expected topology file's runner-class evidence is
// generated from this one producer rather than hand-transcribed. Mirrors
// `netpol-render --print-blob-endpoint`: no cluster touch, no instance
// root, a pure function in, JSON out.
func printE2ERunnerClassJSON(stdout, stderr io.Writer, restrictionsCSV string) int {
	var restrictions []string
	for _, r := range strings.Split(restrictionsCSV, ",") {
		if r = strings.TrimSpace(r); r != "" {
			restrictions = append(restrictions, r)
		}
	}
	canonical := runnercap.CanonicalRestrictions(restrictions)
	out := struct {
		Restrictions []string `json:"restrictions"`
		RunnerClass  string   `json:"runnerClass"`
	}{Restrictions: canonical, RunnerClass: runnercap.RunnerClassValue(restrictions)}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		pf(stderr, "goobers e2e verify: marshal runner class: %v\n", err)
		return 1
	}
	pln(stdout, string(data))
	return 0
}

// e2eSkippedItems reports every item this run of the command did not
// record a verdict for: S3-S7 always (internal/e2e.RequiredSmokeItems'
// S1-S9 set, via Bundle.MissingItems — these need a live topology-driving
// orchestration this command explicitly does not do), S8 whenever
// --expected carried no "liveVisibility" capture (MissingItems catches this
// one too, automatically, since S8 is simply never added in that case),
// plus ARCH11-7/ARCH11-8 when --expected did not supply their data. Never a
// failure by itself — a skipped item was never asked, which
// internal/e2e.Bundle.MissingItems' own doc comment distinguishes from
// asked-and-failed.
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
// the ids it actually recorded (for the human-readable coverage line). It
// is a PURE FUNCTION of its inputs — reads (backed by the run's on-disk
// journal), expected, and now — with no cluster, network, or other live
// dependency, so the emitted Bundle is re-runnable from itself
// (goobernetes-smoke.md §5 rule 4): expectedPath is recorded into
// Collateral precisely so a reader can find --expected again alongside the
// recorded Collateral.RunJournalPaths and reproduce every verdict offline.
// now is injected for deterministic tests.
func e2eBuildBundle(
	ctx context.Context,
	reads readservice.OfflineRuns,
	layout instance.Layout,
	runID, wantGaggle, expectedPath string,
	expected e2eExpectedTopology,
	now func() time.Time,
) (e2e.Bundle, []string, error) {
	detail, err := reads.GetRun(ctx, runID)
	if err != nil {
		return e2e.Bundle{}, nil, fmt.Errorf("get run %q: %w", runID, err)
	}
	if wantGaggle != "" && detail.Gaggle != wantGaggle {
		return e2e.Bundle{}, nil, fmt.Errorf("run %q belongs to gaggle %q, not %q", runID, detail.Gaggle, wantGaggle)
	}

	stages := make([]readservice.AttemptList, 0, len(detail.Stages))
	for _, stageName := range detail.Stages {
		list, err := reads.StageAttempts(ctx, runID, stageName)
		if err != nil {
			return e2e.Bundle{}, nil, fmt.Errorf("stage attempts for %q: %w", stageName, err)
		}
		stages = append(stages, list)
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

	// S8: live-visibility (only when --expected supplies a REAL portal/SSE
	// capture — see e2eLiveVisibilityExpectation's doc comment for why a
	// journal timestamp can never stand in for one).
	if expected.LiveVisibility != nil {
		var terminalAt time.Time
		if detail.FinishedAt != nil {
			terminalAt = *detail.FinishedAt
		}
		observations := make([]e2e.StageTransitionObservation, 0, len(expected.LiveVisibility.Observations))
		for _, o := range expected.LiveVisibility.Observations {
			observations = append(observations, e2e.StageTransitionObservation{
				Source:     expected.LiveVisibility.Source,
				Stage:      o.Stage,
				ObservedAt: o.ObservedAt,
			})
		}
		result := e2e.AssertLiveVisibility(observations, terminalAt)
		if err := add(e2e.ItemS8LiveVisibility, e2e.LiveVisibilityObserver, result); err != nil {
			return e2e.Bundle{}, nil, err
		}
		// §5 rule 4 collateral: --expected is where the raw liveVisibility
		// capture actually lives, so it is the reproducibility pointer.
		bundle.Collateral.S8CapturePath = expectedPath
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
		// The topology file names RESTRICTIONS, never a runner-class label
		// value — this command is the one producer that GENERATES it
		// (internal/runnercap.RunnerClassValue, decision 015, the same
		// function the dispatcher stamps pods with and
		// --print-runner-class exposes standalone), so the evidence
		// records the derived class alongside the triple rather than
		// trusting a hand-transcribed string. Computed (not just
		// omitempty-defaulted) only when the field was actually present in
		// --expected — nil vs. an explicit empty array survives the JSON
		// decode, so an OMITTED field never silently reads as a verified
		// "unrestricted" class.
		evidence := e2eNegativeControlEvidence{Triple: triple}
		if nc.RestrictedRunnerRestrictions != nil {
			evidence.RestrictedRunnerClass = runnercap.RunnerClassValue(nc.RestrictedRunnerRestrictions)
		}
		if nc.ControlVantageRunnerRestrictions != nil {
			evidence.ControlVantageRunnerClass = runnercap.RunnerClassValue(nc.ControlVantageRunnerRestrictions)
		}
		result.Evidence = evidence
		if err := add(e2e.ItemS9NegativeControl, e2e.NegativeControlObserver, result); err != nil {
			return e2e.Bundle{}, nil, err
		}
		// §5 rule 4 collateral: --expected is where the raw S9 probe
		// evidence actually lives, so it is the reproducibility pointer.
		bundle.Collateral.S9ProbeOutputPath = expectedPath
	}

	bundle.FinishedAt = now().UTC()
	return bundle, coverage, nil
}
