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

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readservice"
)

const defaultEscalationListLimit = 50

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
	Verdicts     []verdictView                `json:"verdicts,omitempty"`
}

const escalationsHelp = "Usage: goobers escalations [--json] [--limit=<n>] [--since=<time>] [--api=<url>] [path]\n" +
	"       goobers escalations show [--json] [--include-verdict] [--api=<url>] <run-id> [path]\n" +
	"       goobers escalations resolve --resolution=approve|deny|redirect [flags] <run-id> [path]\n\n" +
	"List escalated runs newest first. Use `escalations show` to inspect an\n" +
	"escalation cause and the artifacts available before and after each stage,\n" +
	"and `escalations resolve` to approve, redirect, or deny one. The list is\n" +
	"bounded to 50 runs by default; use --limit 0 only when an explicit full scan\n" +
	"is acceptable.\n"

func runEscalations(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("escalations", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit escalated runs as JSON")
	limit := fs.Int("limit", defaultEscalationListLimit, "maximum number of escalated runs to show (default: 50; 0 for all)")
	sinceRaw := fs.String("since", "", "only include runs started at or after this time (RFC3339 or YYYY-MM-DD)")
	api := fs.String("api", "", "daemon API base URL for a remote daemon (default $GOOBERS_DAEMON_API)")
	fs.Usage = helpUsage(stderr, "escalations")
	if !parseFlagsBeforePath(fs, args, stderr) {
		return 2
	}
	root, ok := optionalRoot(fs)
	if !ok {
		return 2
	}
	if *limit < 0 {
		pf(stderr, "error: --limit must be non-negative\n")
		return 2
	}
	since, err := parseEscalationSince(*sinceRaw)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	endpoint, err := remoteDaemonAPIBase(*api)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	var reads readservice.OfflineRuns
	if endpoint != "" {
		reads, err = prepareRemoteReads(context.Background(), endpoint, root, fs.NArg() == 1, stderr)
	} else {
		reads, err = newEscalationReads(context.Background(), instance.NewLayout(root), *limit == 0)
	}
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	defer closeEscalationReads(reads)
	items, err := listEscalations(context.Background(), reads, escalationListOptions{
		Limit: *limit,
		Since: since,
	})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if err := confirmRemoteReadIdentity(context.Background(), reads); err != nil {
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

type escalationListOptions struct {
	Limit int
	Since time.Time
}

func parseEscalationSince(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"} {
		parsed, err := time.Parse(layout, raw)
		if err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("--since must be RFC3339 or YYYY-MM-DD")
}

func newEscalationReads(ctx context.Context, layout instance.Layout, allowFullScan bool) (readservice.OfflineRuns, error) {
	if !allowFullScan {
		if _, err := os.Stat(layout.ReadDB()); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("stat read model: %w", err)
			}
			if _, configErr := os.Stat(layout.ConfigFile()); configErr == nil {
				return nil, fmt.Errorf("%w: read model is not available; retry after the daemon builds it or use --limit 0 to opt into a full journal scan", readservice.ErrBoundedReadUnavailable)
			} else if !errors.Is(configErr, os.ErrNotExist) {
				return nil, fmt.Errorf("stat instance config: %w", configErr)
			}
			return readservice.NewOfflineRuns(layout)
		}
		reader, err := readmodel.OpenExistingReader(ctx, layout.ReadDB())
		if err == nil {
			state, stateErr := reader.State(ctx)
			if stateErr != nil {
				_ = reader.Close()
				return nil, fmt.Errorf("inspect read model: %w", stateErr)
			}
			if !state.Ready {
				_ = reader.Close()
				return nil, fmt.Errorf("%w: read model is still building; retry after the daemon finishes or use --limit 0 to opt into a full journal scan", readservice.ErrBoundedReadUnavailable)
			}
			reads, localErr := readservice.NewLocal(readservice.LocalSources{
				Layout:      layout,
				Definitions: &instance.ConfigSet{Manifest: &apiv1.Manifest{}},
				ReadModel:   reader,
			}, func() bool { return true })
			if localErr != nil {
				_ = reader.Close()
				return nil, localErr
			}
			return &closeableOfflineRuns{OfflineRuns: reads, close: reader.Close}, nil
		}
		if !errors.Is(err, readmodel.ErrExistingProjectionUnavailable) {
			return nil, fmt.Errorf("open read model: %w", err)
		}
		return nil, fmt.Errorf("%w: read model cannot serve escalations yet; retry after the daemon rebuilds it or use --limit 0 to opt into a full journal scan", readservice.ErrBoundedReadUnavailable)
	}
	return readservice.NewOfflineRuns(layout)
}

type closeableOfflineRuns struct {
	readservice.OfflineRuns
	close func() error
}

func (c *closeableOfflineRuns) Close() error {
	if c.close == nil {
		return nil
	}
	return c.close()
}

func closeEscalationReads(reads readservice.OfflineRuns) {
	if closer, ok := reads.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
}

const escalationsShowHelp = "Usage: goobers escalations show [--json] [--include-verdict] [--api=<url>] <run-id> [path]\n\n" +
	"Show an escalation's structured cause and per-stage artifact timeline.\n" +
	"Use --include-verdict to include reviewer verdict rationale and findings.\n"

func runEscalationShow(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("escalations show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit the escalation inspection as JSON")
	includeVerdict := fs.Bool("include-verdict", false, "include review verdict content")
	api := fs.String("api", "", "daemon API base URL for a remote daemon (default $GOOBERS_DAEMON_API)")
	fs.Usage = helpUsage(stderr, "escalations show")
	runSelector, root, ok := parseRequiredArgOptionalRoot(fs, args)
	if !ok {
		return 2
	}

	layout := instance.NewLayout(root)
	endpoint, err := remoteDaemonAPIBase(*api)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	var reads readservice.OfflineRuns
	if endpoint != "" {
		reads, err = prepareRemoteReads(context.Background(), endpoint, root, fs.NArg() == 2, stderr)
	} else {
		reads, err = readservice.NewOfflineRuns(layout)
	}
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	var runID string
	if endpoint != "" {
		runID, err = resolveRemoteRunID(context.Background(), reads, runSelector)
	} else {
		runID, err = resolveRunID(layout, runSelector)
	}
	if errors.Is(err, iofs.ErrNotExist) {
		pf(stderr, "error: no run %q found in %s; list escalations with 'goobers escalations'\n", runSelector, root)
		return 1
	}
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	ctx := context.Background()
	detail, err := reads.GetRun(ctx, runID)
	if err != nil {
		pf(stderr, "error: %v\n", err)
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
	if *includeVerdict {
		inspection.Verdicts = loadVerdictViews(ctx, reads, detail.ID, events.Events)
	}
	if err := confirmRemoteReadIdentity(ctx, reads); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if *jsonOutput {
		if err := json.NewEncoder(stdout).Encode(inspection); err != nil {
			pf(stderr, "error: encode escalation: %v\n", err)
			return 2
		}
		return 0
	}
	renderEscalationInspection(stdout, inspection)
	if *includeVerdict {
		pln(stdout, "")
		renderVerdicts(stdout, inspection.Verdicts)
	}
	return 0
}

func listEscalations(ctx context.Context, reads readservice.OfflineRuns, options escalationListOptions) ([]escalationListItem, error) {
	items := make([]escalationListItem, 0)
	cursor := ""
	for {
		limit := options.Limit
		if limit > 0 {
			remaining := limit - len(items)
			if remaining <= 0 {
				break
			}
			if remaining < limit {
				limit = remaining
			}
			if limit > 200 {
				limit = 200
			}
		}
		page, err := reads.ListRuns(ctx, readservice.RunListOptions{
			Phase:  journal.PhaseEscalated,
			Since:  options.Since,
			Limit:  limit,
			Cursor: cursor,
		})
		if err != nil {
			return nil, err
		}
		for _, run := range page.Runs {
			item := escalationListItem{Run: run}
			detail, err := reads.GetRun(ctx, run.ID)
			if err != nil {
				return nil, err
			}
			item.Cause = detail.Escalation
			items = append(items, item)
		}
		if page.NextCursor == "" || (options.Limit > 0 && len(items) >= options.Limit) {
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
		if timeline[i].Status == "running" {
			// #464: a stage still open at escalation (StageStarted with no
			// matching StageFinished) never reaches the finished branch above,
			// so its ArtifactsAfter stayed empty and any artifacts it produced
			// appeared only in the run's current state, not in the timeline.
			// Snapshot the artifacts accumulated up to the escalation point so
			// the unfinished stage's outputs are attributed to it here too.
			timeline[i].ArtifactsAfter = cloneEscalationArtifacts(artifacts)
		}
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
