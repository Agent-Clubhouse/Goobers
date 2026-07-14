package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

func runTrace(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("trace", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		pf(stderr, "Usage: goobers trace <run-id> [path]\n\n"+
			"Show a run's journal events and, if the telemetry rollup has ingested it,\n"+
			"its trace spans (default path \".\"). Exit codes: 0 = OK, 2 = usage/IO error.\n")
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
	pf(stdout, "run:      %s\n", id.RunID)
	pf(stdout, "workflow: %s (v%d)\n", id.Workflow, id.WorkflowVersion)
	if id.WorkflowDigest != "" {
		pf(stdout, "digest:   %s\n", id.WorkflowDigest)
	}
	pf(stdout, "gaggle:   %s\n", id.Gaggle)
	pf(stdout, "trigger:  %s %s\n", id.Trigger.Kind, id.Trigger.Ref)
	pf(stdout, "started:  %s\n", id.StartedAt.Format("2006-01-02T15:04:05Z07:00"))
	if st, err := reader.State(); err == nil {
		pf(stdout, "phase:    %s (machineState=%q, lastSeq=%d)\n", st.Phase, st.MachineState, st.LastSeq)
	}

	events, err := reader.Events()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	pln(stdout, "\nevents:")
	for _, ev := range events {
		pln(stdout, "  "+formatEvent(ev))
	}

	printSpans(stdout, l.TelemetryDB(), id.RunID)
	return 0
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

// printSpans best-effort enriches trace output with rollup-ingested spans. It
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
func printSpans(stdout io.Writer, dbPath, runID string) {
	if _, err := os.Stat(dbPath); err != nil {
		return
	}
	db, err := rollup.Open(dbPath)
	if err != nil {
		return
	}
	defer func() { _ = db.Close() }()

	spans, err := db.Spans(runID)
	if err != nil || len(spans) == 0 {
		return
	}
	pln(stdout, "\nspans:")
	for _, sp := range spans {
		pf(stdout, "  %s status=%s duration=%dms\n", sp.Name, sp.Status, sp.DurationMs)
	}
}
