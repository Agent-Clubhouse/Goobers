package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

const traceTimelineWidth = 120

type traceTimelineStage struct {
	Stage    string                 `json:"stage"`
	Attempts []traceTimelineAttempt `json:"attempts"`
}

type traceTimelineAttempt struct {
	Number         int                  `json:"number,omitempty"`
	Class          string               `json:"class"`
	Repass         bool                 `json:"repass,omitempty"`
	Branch         int                  `json:"branch,omitempty"`
	Status         string               `json:"status,omitempty"`
	StartedAt      *time.Time           `json:"startedAt,omitempty"`
	FinishedAt     *time.Time           `json:"finishedAt,omitempty"`
	DurationMillis *int64               `json:"durationMillis,omitempty"`
	InFlight       bool                 `json:"inFlight,omitempty"`
	Gates          []traceTimelineGate  `json:"gates,omitempty"`
	Usage          []traceTimelineUsage `json:"usage,omitempty"`
	startedSeq     uint64
	finishedSeq    uint64
}

type traceTimelineGate struct {
	Name        string               `json:"name"`
	Verdict     string               `json:"verdict"`
	Target      string               `json:"target,omitempty"`
	EvaluatedAt *time.Time           `json:"evaluatedAt,omitempty"`
	Usage       []traceTimelineUsage `json:"usage,omitempty"`
	startedSeq  uint64
	finishedSeq uint64
}

type traceTimelineUsage struct {
	Model            string   `json:"model,omitempty"`
	InputTokens      *int64   `json:"inputTokens,omitempty"`
	OutputTokens     *int64   `json:"outputTokens,omitempty"`
	CacheReadTokens  *int64   `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens *int64   `json:"cacheWriteTokens,omitempty"`
	ReasoningTokens  *int64   `json:"reasoningTokens,omitempty"`
	Requests         *int64   `json:"requests,omitempty"`
	Cost             *float64 `json:"cost,omitempty"`
	NanoAIU          *float64 `json:"nanoAIU,omitempty"`
}

type traceTerminalCause struct {
	Phase          journal.RunPhase `json:"phase"`
	Stage          string           `json:"stage,omitempty"`
	Gate           string           `json:"gate,omitempty"`
	Code           string           `json:"code,omitempty"`
	Message        string           `json:"message"`
	CausalEventSeq uint64           `json:"causalEventSeq,omitempty"`
}

type timelineAttemptRef struct {
	stage   int
	attempt int
}

type timelineGateStart struct {
	seq uint64
}

func buildTraceTimeline(
	detail readservice.RunDetail,
	events []readservice.RunEvent,
	transcripts []readservice.TranscriptContent,
	now time.Time,
) []traceTimelineStage {
	timeline := make([]traceTimelineStage, 0)
	stageIndexes := make(map[string]int)
	gateStarts := make(map[string]timelineGateStart)
	latestByBranch := make(map[int]timelineAttemptRef)

	stageIndex := func(stage string) int {
		if index, ok := stageIndexes[stage]; ok {
			return index
		}
		index := len(timeline)
		stageIndexes[stage] = index
		timeline = append(timeline, traceTimelineStage{
			Stage:    stage,
			Attempts: []traceTimelineAttempt{},
		})
		return index
	}

	for _, projected := range events {
		if !projected.KnownSchema {
			continue
		}
		event := traceJournalEvent(projected)
		switch event.Type {
		case journal.EventStageStarted:
			index := stageIndex(event.Stage)
			repass := event.AttemptClass == "" && len(timeline[index].Attempts) > 0
			attempt := traceTimelineAttempt{
				Number:     event.Attempt,
				Class:      timelineAttemptClass(event.AttemptClass),
				Repass:     repass,
				Branch:     event.Branch,
				startedSeq: event.Seq,
			}
			if !event.Time.IsZero() {
				started := event.Time
				attempt.StartedAt = &started
			}
			timeline[index].Attempts = append(timeline[index].Attempts, attempt)
			ref := timelineAttemptRef{stage: index, attempt: len(timeline[index].Attempts) - 1}
			latestByBranch[event.Branch] = ref
		case journal.EventStageFinished:
			ref := findTimelineAttempt(timeline, event)
			if ref == nil {
				index := stageIndex(event.Stage)
				timeline[index].Attempts = append(timeline[index].Attempts, traceTimelineAttempt{
					Number: event.Attempt,
					Class:  timelineAttemptClass(event.AttemptClass),
					Branch: event.Branch,
				})
				ref = &timelineAttemptRef{stage: index, attempt: len(timeline[index].Attempts) - 1}
			}
			finishTimelineAttempt(&timeline[ref.stage].Attempts[ref.attempt], event, event.Status)
			latestByBranch[event.Branch] = *ref
		case journal.EventError:
			if event.Stage == "" || event.Error == nil || event.Error.Code != "executor_error" {
				continue
			}
			ref := findTimelineAttempt(timeline, event)
			if ref == nil {
				index := stageIndex(event.Stage)
				timeline[index].Attempts = append(timeline[index].Attempts, traceTimelineAttempt{
					Number: event.Attempt,
					Class:  timelineAttemptClass(event.AttemptClass),
					Branch: event.Branch,
				})
				ref = &timelineAttemptRef{stage: index, attempt: len(timeline[index].Attempts) - 1}
			}
			finishTimelineAttempt(
				&timeline[ref.stage].Attempts[ref.attempt],
				event,
				string(apiv1.ResultFailure),
			)
			latestByBranch[event.Branch] = *ref
		case journal.EventGateStarted:
			gateStarts[timelineGateKey(event.Branch, event.Gate)] = timelineGateStart{
				seq: event.Seq,
			}
		case journal.EventGateEvaluated:
			latest, ok := latestByBranch[event.Branch]
			if !ok {
				continue
			}
			key := timelineGateKey(event.Branch, event.Gate)
			start := gateStarts[key]
			delete(gateStarts, key)
			attempt := &timeline[latest.stage].Attempts[latest.attempt]
			gate := traceTimelineGate{
				Name:        event.Gate,
				Verdict:     event.Verdict,
				Target:      event.Target,
				startedSeq:  start.seq,
				finishedSeq: event.Seq,
			}
			if !event.Time.IsZero() {
				evaluated := event.Time
				gate.EvaluatedAt = &evaluated
			}
			attempt.Gates = append(attempt.Gates, gate)
		}
	}

	if detail.Phase == journal.PhaseRunning {
		for _, ref := range latestByBranch {
			attempt := &timeline[ref.stage].Attempts[ref.attempt]
			if attempt.startedSeq == 0 || attempt.finishedSeq != 0 {
				continue
			}
			attempt.InFlight = true
			if attempt.StartedAt != nil && !now.Before(*attempt.StartedAt) {
				duration := now.Sub(*attempt.StartedAt).Milliseconds()
				attempt.DurationMillis = &duration
			}
		}
	}
	attachTimelineUsage(timeline, transcripts)
	return timeline
}

func findTimelineAttempt(timeline []traceTimelineStage, event journal.Event) *timelineAttemptRef {
	for stageIndex := len(timeline) - 1; stageIndex >= 0; stageIndex-- {
		if timeline[stageIndex].Stage != event.Stage {
			continue
		}
		for attemptIndex := len(timeline[stageIndex].Attempts) - 1; attemptIndex >= 0; attemptIndex-- {
			attempt := timeline[stageIndex].Attempts[attemptIndex]
			if attempt.finishedSeq == 0 &&
				attempt.Number == event.Attempt &&
				attempt.Branch == event.Branch {
				return &timelineAttemptRef{stage: stageIndex, attempt: attemptIndex}
			}
		}
	}
	return nil
}

func finishTimelineAttempt(attempt *traceTimelineAttempt, event journal.Event, status string) {
	attempt.finishedSeq = event.Seq
	attempt.Class = timelineAttemptClass(event.AttemptClass)
	attempt.Status = status
	if event.Time.IsZero() {
		return
	}
	finished := event.Time
	attempt.FinishedAt = &finished
	if attempt.StartedAt != nil && !finished.Before(*attempt.StartedAt) {
		duration := finished.Sub(*attempt.StartedAt).Milliseconds()
		attempt.DurationMillis = &duration
	}
}

func timelineAttemptClass(class journal.AttemptClass) string {
	if class == "" {
		return "initial"
	}
	return string(class)
}

func timelineGateKey(branch int, gate string) string {
	return strconv.Itoa(branch) + ":" + gate
}

func attachTimelineUsage(timeline []traceTimelineStage, transcripts []readservice.TranscriptContent) {
	for _, transcript := range transcripts {
		usage := transcriptUsage(transcript.Bytes)
		if len(usage) == 0 {
			continue
		}
		attached := false
		for stageIndex := range timeline {
			if timeline[stageIndex].Stage == transcript.Stage {
				if attempt := timelineAttemptAtSeq(timeline[stageIndex].Attempts, transcript.Seq); attempt != nil {
					attempt.Usage = mergeTimelineUsage(attempt.Usage, usage)
					attached = true
				}
			}
			if attached {
				break
			}
			for attemptIndex := range timeline[stageIndex].Attempts {
				for gateIndex := range timeline[stageIndex].Attempts[attemptIndex].Gates {
					gate := &timeline[stageIndex].Attempts[attemptIndex].Gates[gateIndex]
					if gate.Name == transcript.Stage &&
						gate.startedSeq > 0 &&
						transcript.Seq > gate.startedSeq &&
						transcript.Seq < gate.finishedSeq {
						gate.Usage = mergeTimelineUsage(gate.Usage, usage)
						attached = true
						break
					}
				}
				if attached {
					break
				}
			}
			if attached {
				break
			}
		}
	}
}

func timelineAttemptAtSeq(attempts []traceTimelineAttempt, seq uint64) *traceTimelineAttempt {
	for index := len(attempts) - 1; index >= 0; index-- {
		attempt := &attempts[index]
		if attempt.startedSeq == 0 || seq <= attempt.startedSeq {
			continue
		}
		if attempt.finishedSeq == 0 || seq < attempt.finishedSeq {
			return attempt
		}
	}
	return nil
}

func transcriptUsage(data []byte) []traceTimelineUsage {
	decoder := json.NewDecoder(bytes.NewReader(data))
	usage := make([]traceTimelineUsage, 0)
	for {
		var event struct {
			Model string `json:"model"`
			Usage *struct {
				InputTokens      *int64   `json:"input_tokens"`
				OutputTokens     *int64   `json:"output_tokens"`
				CacheReadTokens  *int64   `json:"cache_read_tokens"`
				CacheWriteTokens *int64   `json:"cache_write_tokens"`
				ReasoningTokens  *int64   `json:"reasoning_tokens"`
				Requests         *int64   `json:"requests"`
				Cost             *float64 `json:"cost"`
				NanoAIU          *float64 `json:"nano_aiu"`
			} `json:"usage"`
		}
		if err := decoder.Decode(&event); err != nil {
			if err == io.EOF {
				break
			}
			return usage
		}
		if event.Usage == nil {
			continue
		}
		usage = append(usage, traceTimelineUsage{
			Model:            event.Model,
			InputTokens:      event.Usage.InputTokens,
			OutputTokens:     event.Usage.OutputTokens,
			CacheReadTokens:  event.Usage.CacheReadTokens,
			CacheWriteTokens: event.Usage.CacheWriteTokens,
			ReasoningTokens:  event.Usage.ReasoningTokens,
			Requests:         event.Usage.Requests,
			Cost:             event.Usage.Cost,
			NanoAIU:          event.Usage.NanoAIU,
		})
	}
	return usage
}

func mergeTimelineUsage(existing, additions []traceTimelineUsage) []traceTimelineUsage {
	for _, addition := range additions {
		index := -1
		for i := range existing {
			if existing[i].Model == addition.Model {
				index = i
				break
			}
		}
		if index < 0 {
			existing = append(existing, addition)
			continue
		}
		mergeUsageInt(&existing[index].InputTokens, addition.InputTokens)
		mergeUsageInt(&existing[index].OutputTokens, addition.OutputTokens)
		mergeUsageInt(&existing[index].CacheReadTokens, addition.CacheReadTokens)
		mergeUsageInt(&existing[index].CacheWriteTokens, addition.CacheWriteTokens)
		mergeUsageInt(&existing[index].ReasoningTokens, addition.ReasoningTokens)
		mergeUsageInt(&existing[index].Requests, addition.Requests)
		mergeUsageFloat(&existing[index].Cost, addition.Cost)
		mergeUsageFloat(&existing[index].NanoAIU, addition.NanoAIU)
	}
	return existing
}

func mergeUsageInt(total **int64, value *int64) {
	if value == nil {
		return
	}
	if *total == nil {
		copy := *value
		*total = &copy
		return
	}
	**total += *value
}

func mergeUsageFloat(total **float64, value *float64) {
	if value == nil {
		return
	}
	if *total == nil {
		copy := *value
		*total = &copy
		return
	}
	**total += *value
}

func terminalCause(detail readservice.RunDetail, events []readservice.RunEvent) *traceTerminalCause {
	switch detail.Phase {
	case journal.PhaseRunning, journal.PhaseCompleted:
		return nil
	}
	cause := &traceTerminalCause{Phase: detail.Phase}
	if detail.Escalation != nil {
		cause.Message = strings.TrimSpace(detail.Escalation.TerminalReason)
		cause.CausalEventSeq = detail.Escalation.CausalEventSeq
		switch detail.Escalation.Selector.Kind {
		case "gate":
			cause.Gate = detail.Escalation.Selector.Name
		case "stage", "condition":
			cause.Stage = detail.Escalation.Selector.Name
		}
		if cause.Message != "" {
			return cause
		}
	}

	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if !event.KnownSchema {
			continue
		}
		if event.Type != journal.EventError && event.Type != journal.EventRunFinished {
			continue
		}
		if event.Error == nil {
			continue
		}
		cause.Stage = event.Stage
		cause.Gate = event.Gate
		cause.Code = event.Error.Code
		cause.Message = strings.TrimSpace(event.Error.Message)
		cause.CausalEventSeq = event.Seq
		if cause.Message == "" {
			cause.Message = cause.Code
		}
		break
	}
	if cause.Message == "" {
		cause.Message = "(cause not recorded)"
	}
	return cause
}

func printTraceTimeline(stdout io.Writer, timeline []traceTimelineStage, cause *traceTerminalCause) {
	pln(stdout, "timeline:")
	if len(timeline) == 0 {
		pln(stdout, "  (no stage attempts recorded)")
	}
	for _, stage := range timeline {
		parts := []string{fmt.Sprintf("  %s attempts=%d", traceTimelineText(stage.Stage), len(stage.Attempts))}
		for _, attempt := range stage.Attempts {
			parts = append(parts, formatTimelineAttempt(attempt))
			for _, gate := range attempt.Gates {
				parts = append(parts, fmt.Sprintf(
					"gate %s=%s -> %s",
					traceTimelineText(gate.Name),
					traceTimelineText(gate.Verdict),
					traceTimelineText(gate.Target),
				))
				for _, usage := range gate.Usage {
					parts = append(parts, "gate "+traceTimelineText(gate.Name)+" "+formatTimelineUsage(usage))
				}
			}
			for _, usage := range attempt.Usage {
				parts = append(parts, fmt.Sprintf(
					"attempt #%d %s",
					attempt.Number,
					formatTimelineUsage(usage),
				))
			}
		}
		printTimelineLogicalRow(stdout, parts)
	}
	if cause != nil {
		printTimelineLogicalRow(stdout, []string{"  terminal: " + formatTerminalCause(*cause)})
	}
	pln(stdout, "")
}

func formatTimelineAttempt(attempt traceTimelineAttempt) string {
	label := fmt.Sprintf("attempt #%d", attempt.Number)
	switch {
	case attempt.Repass:
		label += " repass"
	case attempt.Class != "initial":
		label += " " + attempt.Class
	}
	duration := "?"
	if attempt.DurationMillis != nil {
		duration = (time.Duration(*attempt.DurationMillis) * time.Millisecond).String()
	}
	status := attempt.Status
	if attempt.InFlight {
		status = "running"
	} else if status == "" {
		status = "partial"
	}
	return fmt.Sprintf("%s %s %s", label, duration, traceTimelineText(status))
}

func formatTimelineUsage(usage traceTimelineUsage) string {
	parts := []string{"usage"}
	if usage.Model != "" {
		parts = append(parts, "model="+traceTimelineText(usage.Model))
	}
	appendInt := func(name string, value *int64) {
		if value != nil {
			parts = append(parts, name+"="+strconv.FormatInt(*value, 10))
		}
	}
	appendFloat := func(name string, value *float64) {
		if value != nil {
			parts = append(parts, name+"="+strconv.FormatFloat(*value, 'f', -1, 64))
		}
	}
	appendInt("in", usage.InputTokens)
	appendInt("out", usage.OutputTokens)
	appendInt("cache-read", usage.CacheReadTokens)
	appendInt("cache-write", usage.CacheWriteTokens)
	appendInt("reasoning", usage.ReasoningTokens)
	appendInt("requests", usage.Requests)
	appendFloat("cost", usage.Cost)
	appendFloat("nano-aiu", usage.NanoAIU)
	return strings.Join(parts, " ")
}

func formatTerminalCause(cause traceTerminalCause) string {
	parts := []string{string(cause.Phase)}
	if cause.Stage != "" {
		parts = append(parts, "stage "+traceTimelineText(cause.Stage))
	}
	if cause.Gate != "" {
		parts = append(parts, "gate "+traceTimelineText(cause.Gate))
	}
	message := traceTimelineText(cause.Message)
	if cause.Code != "" && message != cause.Code {
		message = cause.Code + ": " + message
	}
	parts = append(parts, message)
	return strings.Join(parts, " - ")
}

func printTimelineLogicalRow(stdout io.Writer, parts []string) {
	if len(parts) == 0 {
		return
	}
	line := truncateTimelineText(parts[0], traceTimelineWidth)
	for _, part := range parts[1:] {
		part = truncateTimelineText(traceTimelineText(part), traceTimelineWidth-4)
		next := " | " + part
		if len([]rune(line))+len([]rune(next)) <= traceTimelineWidth {
			line += next
			continue
		}
		pln(stdout, line)
		line = "    " + part
	}
	pln(stdout, line)
}

func traceTimelineText(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func truncateTimelineText(value string, width int) string {
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	if width <= 3 {
		return string(runes[:width])
	}
	return string(runes[:width-3]) + "..."
}
