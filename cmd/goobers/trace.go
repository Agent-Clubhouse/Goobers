package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/internal/workflow"
)

func runTrace(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("trace", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit the run trace as JSON")
	fs.Usage = func() {
		pf(stderr, "Usage: goobers trace [--json] <run-id> [path]\n\n"+
			"Show a run's journal events and, if the telemetry rollup has ingested it,\n"+
			"its trace spans (default path \".\"). Exit codes: 0 = OK, 1 = run not found, 2 = usage/IO error.\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fs.Usage()
		return 2
	}
	runID := fs.Arg(0)
	root := "."
	if fs.NArg() == 2 {
		root = fs.Arg(1)
	}
	// runID is raw CLI input, joined onto RunsDir below — a traversal id
	// (e.g. "../../x") must not read journal-shaped files anywhere outside
	// the instance (#244).
	if !apiv1.ValidRunID(runID) {
		pf(stderr, "error: invalid run id %q\n", runID)
		return 2
	}

	l := instance.NewLayout(root)
	runDir := filepath.Join(l.RunsDir(), runID)
	runInfo, err := os.Stat(runDir)
	if errors.Is(err, os.ErrNotExist) || (err == nil && !runInfo.IsDir()) {
		pf(stderr, "error: no run %q found in %s; list runs with 'goobers status'\n", runID, root)
		return 1
	}
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	id, err := reader.Identity()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	st, stateErr := reader.State()
	phase, err := reader.Phase()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	events, err := reader.Events()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	var escalation *escalationSummary
	if phase == journal.PhaseEscalated {
		summary, err := readEscalationSummary(reader, events)
		if err != nil {
			pf(stderr, "error: escalation summary: %v\n", err)
			return 2
		}
		escalation = &summary
	}

	repasses, err := repassCount(events, "")
	if err != nil {
		pf(stderr, "error: repass count: %v\n", err)
		return 2
	}
	spans := spanSummaries(l.TelemetryDB(), id.RunID)

	var state *journal.State
	if stateErr == nil {
		state = &st
	}
	if *jsonOutput {
		result := traceJSONResult{
			Identity:   id,
			Phase:      phase,
			State:      state,
			Repasses:   repasses,
			Escalation: escalation,
			Events:     events,
			Spans:      spans,
		}
		if err := json.NewEncoder(stdout).Encode(result); err != nil {
			pf(stderr, "error: encode trace: %v\n", err)
			return 2
		}
		return 0
	}

	if escalation != nil {
		printEscalationSummary(stdout, *escalation)
	}
	pf(stdout, "run:      %s\n", id.RunID)
	pf(stdout, "workflow: %s (v%d)\n", id.Workflow, id.WorkflowVersion)
	if id.WorkflowDigest != "" {
		pf(stdout, "digest:   %s\n", id.WorkflowDigest)
	}
	pf(stdout, "gaggle:   %s\n", id.Gaggle)
	pf(stdout, "trigger:  %s %s\n", id.Trigger.Kind, id.Trigger.Ref)
	pf(stdout, "started:  %s\n", id.StartedAt.Format("2006-01-02T15:04:05Z07:00"))
	if state != nil {
		pf(stdout, "phase:    %s (machineState=%q, lastSeq=%d)\n", state.Phase, state.MachineState, state.LastSeq)
	}
	pf(stdout, "repasses: %d\n", repasses)
	pln(stdout, "\nevents:")
	for _, ev := range events {
		pln(stdout, "  "+formatEvent(ev))
	}

	printSpans(stdout, spans)
	return 0
}

type traceJSONResult struct {
	Identity   journal.RunIdentity  `json:"identity"`
	Phase      journal.RunPhase     `json:"phase"`
	State      *journal.State       `json:"state,omitempty"`
	Repasses   int                  `json:"repasses"`
	Escalation *escalationSummary   `json:"escalation,omitempty"`
	Events     []journal.Event      `json:"events"`
	Spans      []rollup.SpanSummary `json:"spans"`
}

type escalationSummary struct {
	Stage                  string `json:"stage"`
	Gate                   string `json:"gate"`
	RepassCount            int    `json:"repassCount"`
	LastNeedsChangesReason string `json:"lastNeedsChangesReason"`
}

func readEscalationSummary(reader *journal.Reader, events []journal.Event) (escalationSummary, error) {
	const notRecorded = "(not recorded)"
	summary := escalationSummary{
		Stage:                  notRecorded,
		Gate:                   notRecorded,
		LastNeedsChangesReason: notRecorded,
	}

	gateIndex := -1
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.Type == journal.EventGateEvaluated && ev.Target == workflow.TargetEscalate {
			gateIndex = i
			summary.Gate = ev.Gate
			break
		}
	}
	if gateIndex < 0 {
		for i := len(events) - 1; i >= 0; i-- {
			if events[i].Type == journal.EventStageFinished {
				summary.Stage = events[i].Stage
				break
			}
		}
		return summary, nil
	}

	for i := gateIndex - 1; i >= 0; i-- {
		if events[i].Type == journal.EventStageFinished {
			summary.Stage = events[i].Stage
			break
		}
	}

	count, err := repassCount(events[:gateIndex+1], summary.Gate)
	if err != nil {
		return escalationSummary{}, err
	}
	summary.RepassCount = count

	for i := gateIndex; i >= 0; i-- {
		ev := events[i]
		if ev.Type != journal.EventGateEvaluated ||
			ev.Gate != summary.Gate ||
			ev.Verdict != string(apiv1.VerdictNeedsChanges) {
			continue
		}
		if ev.Ref == nil {
			break
		}
		data, err := reader.ArtifactBytes(*ev.Ref)
		if err != nil {
			return escalationSummary{}, fmt.Errorf("read verdict for gate %q: %w", summary.Gate, err)
		}
		var verdict apiv1.Verdict
		if err := json.Unmarshal(data, &verdict); err != nil {
			return escalationSummary{}, fmt.Errorf("parse verdict for gate %q: %w", summary.Gate, err)
		}
		if verdict.Decision != apiv1.VerdictNeedsChanges {
			return escalationSummary{}, fmt.Errorf(
				"verdict artifact for gate %q has decision %q, want %q",
				summary.Gate, verdict.Decision, apiv1.VerdictNeedsChanges,
			)
		}
		summary.LastNeedsChangesReason = strings.TrimSpace(verdict.Rationale)
		if summary.LastNeedsChangesReason == "" {
			summary.LastNeedsChangesReason = strings.TrimSpace(verdict.Summary)
		}
		if summary.LastNeedsChangesReason == "" {
			summary.LastNeedsChangesReason = notRecorded
		}
		break
	}
	return summary, nil
}

// repassTargetStage is the stage a gate's non-pass verdict routes back to
// for another implementation attempt. Hardcoded rather than derived from the
// workflow definition — trace.go reads only the run's journal, never the
// workflow config, and every V0 workflow on the repass path today names its
// retry stage "implement". Revisit if a future workflow gives its repass
// target a different name (#354).
const repassTargetStage = "implement"

// repassCount is the single source of truth for "how many times has this
// run repassed," used both for the whole-run header line (gate == "") and
// the escalation-block per-gate count (gate == the escalating gate's name),
// so the two numbers are computed by one rule instead of two independently
// maintained implementations that could silently disagree (#354).
//
// gate == "": a whole-run total — every gate.evaluated event, from any gate,
// whose Target routes back to repassTargetStage. This can exceed a single
// gate's own count when more than one gate repassed during the run.
//
// gate != "": scoped to one gate, restricted to the given events (callers
// pass the prefix up to and including that gate's terminal evaluation).
// Prefers the terminal event's Runner["repassAttempt"] when present — the
// runner's own authoritative count — else counts that gate's own
// needs-changes verdicts sequentially, resetting to 0 on a pass (a streak
// since the last pass, not a running total).
func repassCount(events []journal.Event, gate string) (int, error) {
	if gate == "" {
		count := 0
		for _, ev := range events {
			if ev.Type == journal.EventGateEvaluated && ev.Target == repassTargetStage {
				count++
			}
		}
		return count, nil
	}

	if len(events) == 0 {
		return 0, fmt.Errorf("repass count for gate %q: no events", gate)
	}
	if raw, ok := events[len(events)-1].Runner["repassAttempt"]; ok {
		data, err := json.Marshal(raw)
		if err != nil {
			return 0, fmt.Errorf("marshal repass count for gate %q: %w", gate, err)
		}
		var count int
		if err := json.Unmarshal(data, &count); err != nil {
			return 0, fmt.Errorf("parse repass count for gate %q: %w", gate, err)
		}
		if count < 0 {
			return 0, fmt.Errorf("invalid repass count %d for gate %q", count, gate)
		}
		return count, nil
	}

	count := 0
	for _, ev := range events {
		if ev.Type != journal.EventGateEvaluated || ev.Gate != gate {
			continue
		}
		if ev.Verdict == string(apiv1.VerdictPass) {
			count = 0
		} else {
			count++
		}
	}
	return count, nil
}

func printEscalationSummary(stdout io.Writer, summary escalationSummary) {
	reason := strings.ReplaceAll(summary.LastNeedsChangesReason, "\n", "\n    ")
	pf(stdout, "⚠ ESCALATED\n")
	pf(stdout, "  stage: %s\n", summary.Stage)
	pf(stdout, "  gate: %s\n", summary.Gate)
	pf(stdout, "  repass count: %d\n", summary.RepassCount)
	pf(stdout, "  last needs-changes reason: %s\n\n", reason)
}

// formatEvent renders one journal event as a single debug line, matching the
// per-type field groupings documented in internal/journal/README.md's
// cat/jq debugging section so `trace` output reads the same as `jq`-ing the
// raw events.jsonl by hand.
func formatEvent(ev journal.Event) string {
	prefix := fmt.Sprintf("[%d] %s", ev.Seq, ev.Type)
	switch ev.Type {
	case journal.EventStageStarted, journal.EventStageFinished:
		s := fmt.Sprintf("%s stage=%s attempt=%d", prefix, ev.Stage, ev.Attempt)
		if ev.AttemptClass != "" {
			s += fmt.Sprintf(" class=%s", ev.AttemptClass)
		}
		if ev.Status != "" {
			s += fmt.Sprintf(" status=%s", ev.Status)
		}
		return s
	case journal.EventGateEvaluated:
		return fmt.Sprintf("%s gate=%s verdict=%s target=%s", prefix, ev.Gate, ev.Verdict, ev.Target)
	case journal.EventArtifactRecorded, journal.EventInputSnapshot:
		s := fmt.Sprintf("%s name=%s", prefix, ev.Name)
		if ev.Ref != nil {
			s += fmt.Sprintf(" digest=%s size=%d", ev.Ref.Digest, ev.Ref.Size)
		}
		return s
	case journal.EventRefTouched:
		if ev.ExternalRef != nil {
			return fmt.Sprintf("%s provider=%s kind=%s id=%s url=%s",
				prefix, ev.ExternalRef.Provider, ev.ExternalRef.Kind, ev.ExternalRef.ID, ev.ExternalRef.URL)
		}
		return prefix
	case journal.EventError:
		if ev.Error != nil {
			return fmt.Sprintf("%s code=%s message=%q", prefix, ev.Error.Code, ev.Error.Message)
		}
		return prefix
	case journal.EventRedaction:
		if ev.Redaction != nil {
			return fmt.Sprintf("%s target=%s old=%s new=%s", prefix, ev.Redaction.Target, ev.Redaction.OldDigest, ev.Redaction.NewDigest)
		}
		return prefix
	case journal.EventRunStarted, journal.EventRunFinished:
		if ev.Status != "" {
			return fmt.Sprintf("%s status=%s", prefix, ev.Status)
		}
		return prefix
	default:
		return prefix
	}
}

// spanSummaries best-effort loads rollup-ingested spans. It
// reads telemetry.db directly (no Rebuild call here) — that used to mean a
// fresh `goobers trace` right after `goobers run` showed nothing until a
// separate `goobers telemetry stats/errors` had rebuilt the db first (issue
// #129's checklist). That gap closed as a side effect of #127/#128's
// incremental-ingest wiring: `goobers run`/`up` now call IngestRun on every
// run finish, so telemetry.db already has this run's spans by the time
// `trace` reads it — no explicit rebuild step needed. A missing telemetry.db
// (telemetry disabled, issue #129's telemetry.enabled) is still not an
// error — spans are informational only (ARCHITECTURE.md §3.3 excludes them
// from conformance) — so this silently does nothing rather than requiring
// rollup setup just to read a run's journal.
func spanSummaries(dbPath, runID string) []rollup.SpanSummary {
	empty := []rollup.SpanSummary{}
	if _, err := os.Stat(dbPath); err != nil {
		return empty
	}
	db, err := rollup.Open(dbPath)
	if err != nil {
		return empty
	}
	defer func() { _ = db.Close() }()

	spans, err := db.Spans(runID)
	if err != nil || spans == nil {
		return empty
	}
	return spans
}

func printSpans(stdout io.Writer, spans []rollup.SpanSummary) {
	if len(spans) == 0 {
		return
	}
	pln(stdout, "\nspans:")
	for _, sp := range spans {
		pf(stdout, "  %s status=%s duration=%dms\n", sp.Name, sp.Status, sp.DurationMs)
	}
}
