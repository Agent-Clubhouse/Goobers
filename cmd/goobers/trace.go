package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

func runTrace(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("trace", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit the run trace as JSON")
	showTranscripts := fs.Bool("transcripts", false, "show every recorded agent-stage transcript")
	transcriptStage := fs.String("transcript", "", "show recorded transcript data for one stage")
	fs.Usage = func() {
		pf(stderr, "Usage: goobers trace [--json] [--transcripts | --transcript=<stage>] <run-id> [path]\n\n"+
			"Show a run's journal events and, if the telemetry rollup has ingested it,\n"+
			"its trace spans. Use --transcripts to show all recorded agent transcripts,\n"+
			"or --transcript to select one stage (default path \".\"). Exit codes:\n"+
			"0 = OK, 1 = run/transcript not found, 2 = usage/IO error.\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	transcriptSelected := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "transcript" {
			transcriptSelected = true
		}
	})
	if *showTranscripts && transcriptSelected {
		pf(stderr, "error: --transcripts and --transcript cannot be used together\n")
		return 2
	}
	selectedStage := strings.TrimSpace(*transcriptStage)
	if transcriptSelected && selectedStage == "" {
		pf(stderr, "error: --transcript requires a stage name\n")
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
	// Reject traversal-shaped prefixes before asking the shared service to
	// inspect or list any run.
	if !apiv1.ValidRunID(runID) {
		pf(stderr, "error: invalid run id %q\n", runID)
		return 2
	}

	l := instance.NewLayout(root)
	reads, err := readservice.NewOfflineRuns(l)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	ctx := context.Background()
	detail, matches, err := resolveTraceRun(ctx, reads, runID)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	switch len(matches) {
	case 0:
		if detail.ID == "" {
			pf(stderr, "error: no run %q found in %s; list runs with 'goobers status'\n", runID, root)
			return 1
		}
	case 1:
		detail, err = reads.GetRun(ctx, matches[0])
		if err != nil {
			pf(stderr, "error: %v\n", err)
			return 2
		}
	default:
		pf(stderr, "error: ambiguous prefix %q matches %d runs: %s\n", runID, len(matches), strings.Join(matches, ", "))
		return 2
	}
	runID = detail.ID

	if *showTranscripts || transcriptSelected {
		transcripts, err := reads.RunTranscripts(ctx, runID, selectedStage)
		if err != nil {
			pf(stderr, "error: %v in run %q\n", err, runID)
			return 2
		}
		if err := printTranscripts(stdout, transcripts, selectedStage); err != nil {
			pf(stderr, "error: %v in run %q\n", err, runID)
			if errors.Is(err, errTranscriptNotFound) {
				return 1
			}
			return 2
		}
		return 0
	}

	ledger, err := reads.RunEvents(ctx, runID)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	identity, state, err := reads.RunMetadata(ctx, runID)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	spans, err := reads.RunSpans(ctx, runID)
	if err != nil {
		// Spans are informational and have always been best-effort for
		// telemetry-disabled, missing, or unreadable rollups.
		spans = []rollup.SpanSummary{}
	}
	traceEscalationDetail, err := reads.RunEscalation(ctx, runID)
	if err != nil {
		pf(stderr, "error: escalation summary: %v\n", err)
		return 2
	}
	repasses, err := reads.RunTraceRepassCount(ctx, runID)
	if err != nil {
		pf(stderr, "error: repass count: %v\n", err)
		return 2
	}
	escalation := traceEscalation(detail, traceEscalationDetail, ledger.Events)
	if *jsonOutput {
		result := traceJSONResult{
			Identity:   identity,
			Phase:      detail.Phase,
			State:      state,
			Repasses:   repasses,
			Escalation: escalation,
			Events:     traceJSONEvents(ledger.Events),
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
	pf(stdout, "run:      %s\n", detail.ID)
	pf(stdout, "workflow: %s (v%d)\n", detail.Workflow, detail.WorkflowVersion)
	if detail.WorkflowDigest != "" {
		pf(stdout, "digest:   %s\n", detail.WorkflowDigest)
	}
	pf(stdout, "gaggle:   %s\n", detail.Gaggle)
	pf(stdout, "trigger:  %s %s\n", detail.Trigger.Kind, detail.Trigger.Ref)
	pf(stdout, "started:  %s\n", detail.StartedAt.Format("2006-01-02T15:04:05Z07:00"))
	if state != nil {
		pf(stdout, "phase:    %s (machineState=%q, lastSeq=%d)\n", state.Phase, state.MachineState, state.LastSeq)
	}
	pf(stdout, "repasses: %d\n", repasses)
	pln(stdout, "\nevents:")
	for _, event := range ledger.Events {
		pln(stdout, "  "+formatEvent(traceJournalEvent(event)))
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
	Events     []traceJSONEvent     `json:"events"`
	Spans      []rollup.SpanSummary `json:"spans"`
}

type traceJSONEvent struct {
	journal.Event
	KnownSchema *bool           `json:"knownSchema,omitempty"`
	Raw         json.RawMessage `json:"raw,omitempty"`
}

var errTranscriptNotFound = errors.New("no recorded agent transcript")

func printTranscripts(stdout io.Writer, transcripts []readservice.TranscriptContent, stage string) error {
	if len(transcripts) == 0 {
		if stage != "" {
			return fmt.Errorf("%w for stage %q", errTranscriptNotFound, stage)
		}
		return errTranscriptNotFound
	}

	pln(stdout, "transcripts:")
	for i, transcript := range transcripts {
		if i > 0 {
			pln(stdout, "")
		}
		pf(stdout, "--- stage=%q name=%q seq=%d ---\n",
			transcript.Stage, transcript.Name, transcript.Seq)
		pf(stdout, "%s", transcript.Bytes)
		if transcript.Bytes[len(transcript.Bytes)-1] != '\n' {
			pln(stdout, "")
		}
	}
	return nil
}

type escalationSummary struct {
	Stage                  string `json:"stage"`
	Gate                   string `json:"gate"`
	RepassCount            int    `json:"repassCount"`
	LastNeedsChangesReason string `json:"lastNeedsChangesReason"`
}

func traceEscalation(
	detail readservice.RunDetail,
	traceDetail *readservice.TraceEscalation,
	events []readservice.RunEvent,
) *escalationSummary {
	if detail.Escalation == nil {
		return nil
	}
	const notRecorded = "(not recorded)"
	summary := escalationSummary{
		Stage:                  notRecorded,
		Gate:                   notRecorded,
		RepassCount:            detail.Escalation.RepassCount,
		LastNeedsChangesReason: notRecorded,
	}
	if traceDetail != nil {
		summary.RepassCount = traceDetail.RepassCount
		if traceDetail.LastNeedsChangesReason != "" {
			summary.LastNeedsChangesReason = traceDetail.LastNeedsChangesReason
		}
	}
	switch detail.Escalation.Selector.Kind {
	case "gate":
		summary.Gate = detail.Escalation.Selector.Name
	case "stage":
		summary.Stage = detail.Escalation.Selector.Name
	}
	if summary.Stage == notRecorded {
		for i := len(events) - 1; i >= 0; i-- {
			event := events[i]
			if event.Seq >= detail.Escalation.CausalEventSeq ||
				!event.KnownSchema ||
				event.Type != journal.EventStageFinished {
				continue
			}
			summary.Stage = event.Stage
			break
		}
	}
	return &summary
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
		if len(ev.Outputs) > 0 {
			outputs, err := json.Marshal(ev.Outputs)
			if err != nil {
				s += " outputs=<invalid>"
			} else {
				s += " outputs=" + string(outputs)
			}
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

func resolveTraceRun(ctx context.Context, reads readservice.OfflineRuns, runID string) (readservice.RunDetail, []string, error) {
	detail, err := reads.GetRun(ctx, runID)
	if err == nil {
		return detail, nil, nil
	}
	if !errors.Is(err, readservice.ErrNotFound) {
		return readservice.RunDetail{}, nil, err
	}

	ids, err := reads.RunIDs(ctx)
	if err != nil {
		return readservice.RunDetail{}, nil, err
	}
	matches := make([]string, 0)
	for _, id := range ids {
		if strings.HasPrefix(id, runID) {
			matches = append(matches, id)
		}
	}
	return readservice.RunDetail{}, matches, nil
}

func traceJSONEvents(events []readservice.RunEvent) []traceJSONEvent {
	result := make([]traceJSONEvent, len(events))
	for i, event := range events {
		result[i].Event = traceJournalEvent(event)
		if !event.KnownSchema {
			known := false
			result[i].KnownSchema = &known
			result[i].Raw = event.Raw
		}
	}
	return result
}

func traceJournalEvent(event readservice.RunEvent) journal.Event {
	if event.JournalEvent != nil {
		return *event.JournalEvent
	}
	attemptClass := journal.AttemptClass(event.AttemptClass)
	if event.AttemptClass == "initial" {
		attemptClass = ""
	}
	projected := journal.Event{
		Schema:       event.Schema,
		Seq:          event.Seq,
		Type:         event.Type,
		Branch:       event.Branch,
		Time:         event.Time,
		Stage:        event.Stage,
		Attempt:      event.Attempt,
		AttemptClass: attemptClass,
		Gate:         event.Gate,
		Verdict:      event.Verdict,
		Target:       event.Target,
		Status:       event.Status,
		Outputs:      event.Outputs,
		Name:         event.Name,
		ExternalRef:  event.ExternalRef,
		Error:        event.Error,
		Redaction:    event.Redaction,
		Runner:       event.Runner,
		Workflow:     event.Workflow,
		RunID:        event.RunID,
		Reason:       event.Reason,
	}
	if event.Artifact != nil {
		projected.Ref = &journal.Ref{
			Digest:    event.Artifact.Digest,
			Size:      event.Artifact.Size,
			MediaType: event.Artifact.MediaType,
		}
	}
	return projected
}

func printSpans(stdout io.Writer, spans []rollup.SpanSummary) {
	if len(spans) == 0 {
		return
	}
	pln(stdout, "\nspans:")
	for _, sp := range spans {
		// business=%s (issue #710) shows the run/stage's actual outcome
		// alongside OTel's own coarser status — the two use different
		// vocabularies (ok/error vs success/failed/completed/escalated/...),
		// so a business-failed span now reads "status=error business=failed"
		// instead of the pre-fix "status=ok" a failed run misleadingly wore.
		// Empty for a span that never calls Span.Complete (a gate span,
		// still Succeed/Fail) or one predating this fix.
		suffix := ""
		if sp.BusinessStatus != "" {
			suffix = " business=" + sp.BusinessStatus
		}
		pf(stdout, "  %s status=%s%s duration=%dms\n", sp.Name, sp.Status, suffix, sp.DurationMs)
	}
}
