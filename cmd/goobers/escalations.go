package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

const escalationListPageSize = 200

type escalationListItem struct {
	Run   readservice.RunSummary       `json:"run"`
	Cause *readservice.EscalationCause `json:"cause,omitempty"`
}

type escalationListResult struct {
	Escalations []escalationListItem `json:"escalations"`
}

type escalationArtifactStep struct {
	Stage           string                         `json:"stage"`
	Branch          int                            `json:"branch"`
	Attempt         int                            `json:"attempt"`
	AttemptClass    string                         `json:"attemptClass"`
	Status          string                         `json:"status"`
	StartedSeq      uint64                         `json:"startedSeq,omitempty"`
	FinishedSeq     uint64                         `json:"finishedSeq,omitempty"`
	StartedAt       *time.Time                     `json:"startedAt,omitempty"`
	FinishedAt      *time.Time                     `json:"finishedAt,omitempty"`
	ArtifactsBefore []readservice.ArtifactMetadata `json:"artifactsBefore"`
	ArtifactsAfter  []readservice.ArtifactMetadata `json:"artifactsAfter"`
}

type escalationCurrentState struct {
	Phase     journal.RunPhase               `json:"phase"`
	Artifacts []readservice.ArtifactMetadata `json:"artifacts"`
}

type escalationInspection struct {
	Run          readservice.RunSummary       `json:"run"`
	Cause        *readservice.EscalationCause `json:"cause,omitempty"`
	Timeline     []escalationArtifactStep     `json:"timeline"`
	CurrentState escalationCurrentState       `json:"currentState"`
}

func runEscalations(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("escalations", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit escalated runs as JSON")
	fs.Usage = func() {
		pf(stderr, "Usage: goobers escalations [--json] [path]\n"+
			"       goobers escalations show [--json] <run-id> [path]\n\n"+
			"List escalated runs newest first. Use `escalations show` to inspect an\n"+
			"escalation cause and the artifacts available before and after each stage.\n")
	}
	if err := fs.Parse(args); err != nil {
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

	reads, err := readservice.NewOfflineRuns(instance.NewLayout(root))
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	items, err := listEscalations(context.Background(), reads)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if *jsonOutput {
		if err := json.NewEncoder(stdout).Encode(escalationListResult{Escalations: items}); err != nil {
			pf(stderr, "error: encode escalations: %v\n", err)
			return 2
		}
		return 0
	}
	renderEscalationList(stdout, items)
	return 0
}

func runEscalationShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("escalations show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit the escalation inspection as JSON")
	fs.Usage = func() {
		pf(stderr, "Usage: goobers escalations show [--json] <run-id> [path]\n\n"+
			"Show an escalation's structured cause and per-stage artifact timeline.\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fs.Usage()
		return 2
	}
	runID := fs.Arg(0)
	if !apiv1.ValidRunID(runID) {
		pf(stderr, "error: invalid run id %q\n", runID)
		return 2
	}
	root := "."
	if fs.NArg() == 2 {
		root = fs.Arg(1)
	}

	reads, err := readservice.NewOfflineRuns(instance.NewLayout(root))
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
			pf(stderr, "error: no run %q found in %s; list escalations with 'goobers escalations'\n", runID, root)
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
	if detail.Phase != journal.PhaseEscalated {
		pf(stderr, "error: run %q has phase %s, not escalated\n", detail.ID, detail.Phase)
		return 1
	}

	events, err := reads.RunEvents(ctx, detail.ID)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	inspection := inspectEscalation(detail, events.Events)
	if *jsonOutput {
		if err := json.NewEncoder(stdout).Encode(inspection); err != nil {
			pf(stderr, "error: encode escalation: %v\n", err)
			return 2
		}
		return 0
	}
	renderEscalationInspection(stdout, inspection)
	return 0
}

func listEscalations(ctx context.Context, reads readservice.OfflineRuns) ([]escalationListItem, error) {
	items := make([]escalationListItem, 0)
	cursor := ""
	for {
		page, err := reads.ListRuns(ctx, readservice.RunListOptions{
			Phase:  journal.PhaseEscalated,
			Limit:  escalationListPageSize,
			Cursor: cursor,
		})
		if err != nil {
			return nil, err
		}
		for _, run := range page.Runs {
			detail, err := reads.GetRun(ctx, run.ID)
			if err != nil {
				return nil, err
			}
			items = append(items, escalationListItem{Run: run, Cause: detail.Escalation})
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return items, nil
}

func inspectEscalation(detail readservice.RunDetail, events []readservice.RunEvent) escalationInspection {
	timeline, artifacts := escalationArtifactTimeline(events)
	return escalationInspection{
		Run:      detail.RunSummary,
		Cause:    detail.Escalation,
		Timeline: timeline,
		CurrentState: escalationCurrentState{
			Phase:     detail.Phase,
			Artifacts: artifacts,
		},
	}
}

func escalationArtifactTimeline(events []readservice.RunEvent) ([]escalationArtifactStep, []readservice.ArtifactMetadata) {
	timeline := make([]escalationArtifactStep, 0)
	artifacts := make([]readservice.ArtifactMetadata, 0)
	for _, event := range events {
		if !event.KnownSchema {
			continue
		}
		if event.Artifact != nil {
			artifacts = updateEscalationArtifact(artifacts, *event.Artifact)
		}
		for _, artifact := range event.Artifacts {
			artifacts = updateEscalationArtifact(artifacts, artifact)
		}

		switch event.Type {
		case journal.EventStageStarted:
			started := event.Time
			timeline = append(timeline, escalationArtifactStep{
				Stage:           event.Stage,
				Branch:          event.Branch,
				Attempt:         event.Attempt,
				AttemptClass:    event.AttemptClass,
				Status:          "running",
				StartedSeq:      event.Seq,
				StartedAt:       &started,
				ArtifactsBefore: cloneEscalationArtifacts(artifacts),
				ArtifactsAfter:  []readservice.ArtifactMetadata{},
			})
		case journal.EventStageFinished:
			index := openEscalationStep(timeline, event)
			if index < 0 {
				timeline = append(timeline, escalationArtifactStep{
					Stage:           event.Stage,
					Branch:          event.Branch,
					Attempt:         event.Attempt,
					AttemptClass:    event.AttemptClass,
					ArtifactsBefore: cloneEscalationArtifacts(artifacts),
				})
				index = len(timeline) - 1
			}
			finished := event.Time
			timeline[index].Status = event.Status
			timeline[index].FinishedSeq = event.Seq
			timeline[index].FinishedAt = &finished
			timeline[index].ArtifactsAfter = cloneEscalationArtifacts(artifacts)
		}
	}
	for i := range timeline {
		if timeline[i].ArtifactsAfter == nil {
			timeline[i].ArtifactsAfter = []readservice.ArtifactMetadata{}
		}
	}
	return timeline, cloneEscalationArtifacts(artifacts)
}

func openEscalationStep(timeline []escalationArtifactStep, event readservice.RunEvent) int {
	fallback := -1
	for i := len(timeline) - 1; i >= 0; i-- {
		step := timeline[i]
		if step.FinishedSeq != 0 ||
			step.Stage != event.Stage ||
			step.Branch != event.Branch ||
			step.Attempt != event.Attempt {
			continue
		}
		if step.AttemptClass == event.AttemptClass {
			return i
		}
		if fallback < 0 {
			fallback = i
		}
	}
	return fallback
}

func updateEscalationArtifact(
	artifacts []readservice.ArtifactMetadata,
	artifact readservice.ArtifactMetadata,
) []readservice.ArtifactMetadata {
	for i := range artifacts {
		if artifacts[i].RecordedSeq == artifact.RecordedSeq {
			artifacts[i] = artifact
			return artifacts
		}
	}
	return append(artifacts, artifact)
}

func cloneEscalationArtifacts(artifacts []readservice.ArtifactMetadata) []readservice.ArtifactMetadata {
	if len(artifacts) == 0 {
		return []readservice.ArtifactMetadata{}
	}
	return append([]readservice.ArtifactMetadata(nil), artifacts...)
}

func renderEscalationList(stdout io.Writer, items []escalationListItem) {
	if len(items) == 0 {
		pln(stdout, "no escalated runs found")
		return
	}
	pf(stdout, "%-34s  %-24s  %-20s  %-8s  %-7s  %s\n",
		"RUN ID", "WORKFLOW", "CAUSE", "REPASSES", "RETRIES", "STARTED")
	for _, item := range items {
		selector := escalationSelectorText(item.Cause)
		repasses, retries := 0, 0
		if item.Cause != nil {
			repasses = item.Cause.RepassCount
			retries = item.Cause.RetryCount
		}
		pf(stdout, "%-34s  %-24s  %-20s  %-8d  %-7d  %s\n",
			item.Run.ID, item.Run.Workflow, selector, repasses, retries, item.Run.StartedAt.Format(time.RFC3339))
		if item.Cause != nil && item.Cause.TerminalReason != "" {
			pf(stdout, "  cause: %s\n", strings.ReplaceAll(item.Cause.TerminalReason, "\n", " "))
		}
	}
}

func renderEscalationInspection(stdout io.Writer, inspection escalationInspection) {
	pf(stdout, "run:      %s\n", inspection.Run.ID)
	pf(stdout, "workflow: %s (v%d)\n", inspection.Run.Workflow, inspection.Run.WorkflowVersion)
	pf(stdout, "phase:    %s\n", inspection.Run.Phase)
	pf(stdout, "started:  %s\n", inspection.Run.StartedAt.Format(time.RFC3339))
	pln(stdout, "\ncause:")
	if inspection.Cause == nil {
		pln(stdout, "  (not recorded)")
	} else {
		pf(stdout, "  selector: %s\n", escalationSelectorText(inspection.Cause))
		if inspection.Cause.SelectedBranch != "" {
			pf(stdout, "  branch: %s\n", inspection.Cause.SelectedBranch)
		}
		pf(stdout, "  repasses: %d\n", inspection.Cause.RepassCount)
		pf(stdout, "  retries: %d\n", inspection.Cause.RetryCount)
		if inspection.Cause.TerminalReason != "" {
			pf(stdout, "  reason: %s\n", strings.ReplaceAll(inspection.Cause.TerminalReason, "\n", "\n    "))
		}
	}

	pln(stdout, "\nartifact timeline:")
	if len(inspection.Timeline) == 0 {
		pln(stdout, "  no stage events recorded")
	}
	for _, step := range inspection.Timeline {
		pf(stdout, "  stage=%s attempt=%d class=%s status=%s seq=%d-%d\n",
			step.Stage, step.Attempt, step.AttemptClass, step.Status, step.StartedSeq, step.FinishedSeq)
		renderEscalationArtifacts(stdout, "before", step.ArtifactsBefore)
		renderEscalationArtifacts(stdout, "after", step.ArtifactsAfter)
	}

	pln(stdout, "\ncurrent state:")
	pf(stdout, "  phase: %s\n", inspection.CurrentState.Phase)
	renderEscalationArtifacts(stdout, "artifacts", inspection.CurrentState.Artifacts)
}

func renderEscalationArtifacts(stdout io.Writer, label string, artifacts []readservice.ArtifactMetadata) {
	if len(artifacts) == 0 {
		pf(stdout, "    %s: (none)\n", label)
		return
	}
	pf(stdout, "    %s:\n", label)
	for _, artifact := range artifacts {
		name := artifact.Name
		if name == "" {
			name = "(unnamed)"
		}
		pf(stdout, "      %s digest=%s size=%d mediaType=%s\n",
			name, artifact.Digest, artifact.Size, artifact.MediaType)
	}
}

func escalationSelectorText(cause *readservice.EscalationCause) string {
	if cause == nil || cause.Selector.Kind == "" {
		return "(not recorded)"
	}
	if cause.Selector.Name == "" {
		return cause.Selector.Kind
	}
	return cause.Selector.Kind + "/" + cause.Selector.Name
}
