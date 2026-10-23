package main

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

// fidelityLevel names how much of a case's recorded tool evidence an
// evidence-fidelity ablation (#7317) leaves in place. Every level only
// removes recorded observations or marks them explicitly as missing; none
// fabricates an observation the adapter did not record.
type fidelityLevel string

const (
	// fidelityFull is the case exactly as recorded.
	fidelityFull fidelityLevel = "full"
	// fidelityOutcomeAblated keeps every tool call and result record but
	// removes each result's recorded outcome (success), so the call's
	// identity survives and its outcome is unknown.
	fidelityOutcomeAblated fidelityLevel = "outcome-ablated"
	// fidelityResultsTruncated cuts each transcript at its first tool result
	// and appends an explicit truncation marker, the record shape the harness
	// writes when it bounds a canonical transcript.
	fidelityResultsTruncated fidelityLevel = "results-truncated"
	// fidelityTranscriptMissing withholds each transcript's content while its
	// span.recorded event stays in the journal, which is how a recorded span
	// whose content cannot be retrieved looks to Backprop.
	fidelityTranscriptMissing fidelityLevel = "transcript-missing"
)

// fidelityLevels are the ablation levels, most evidence first.
var fidelityLevels = []fidelityLevel{
	fidelityFull, fidelityOutcomeAblated, fidelityResultsTruncated, fidelityTranscriptMissing,
}

// truncationMarker is the explicit marker a results-truncated transcript
// ends with, in the canonical transcript's truncation-record shape.
const truncationMarker = `{"role":"system","content":"[transcript truncated: evidence ablation]","truncated":true}`

// unidentifiedAdapter groups transcripts whose span name does not identify
// the adapter that recorded them.
const unidentifiedAdapter = "unidentified"

// unrecordedToolObservations are the tool observations the canonical
// transcript (goobers.dev/telemetry/genai-event/v1) has no field for, for
// any adapter. The ablation cannot measure their contribution, so the report
// states them rather than implying they were observed.
var unrecordedToolObservations = []string{"start", "end", "duration", "resultDigest", "evidenceSource"}

// evidenceChange counts what an ablation removed from one case.
type evidenceChange struct {
	TranscriptsAffected int `json:"transcriptsAffected"`
	OutcomesRemoved     int `json:"outcomesRemoved"`
	RecordsDropped      int `json:"recordsDropped"`
	TranscriptsWithheld int `json:"transcriptsWithheld"`
}

func (change evidenceChange) any() bool {
	return change.OutcomesRemoved > 0 || change.RecordsDropped > 0 || change.TranscriptsWithheld > 0
}

// transcriptRecord is the part of a canonical transcript record the ablation
// reads.
type transcriptRecord struct {
	Role     string `json:"role"`
	ToolCall *struct {
		ID      string `json:"id"`
		Success *bool  `json:"success,omitempty"`
	} `json:"tool_call,omitempty"`
	Truncated bool `json:"truncated,omitempty"`
}

func (record transcriptRecord) isToolResult() bool {
	return record.ToolCall != nil && record.Role == "tool"
}

// ablate returns a copy of input at the given fidelity and what it removed.
// The journal is shared, never modified; only span content changes.
func ablate(input evalInput, level fidelityLevel) (evalInput, evidenceChange) {
	ablated := input
	ablated.SpanData = make(map[string]string, len(input.SpanData))
	for digest, content := range input.SpanData {
		ablated.SpanData[digest] = content
	}
	var change evidenceChange
	if level == fidelityFull {
		return ablated, change
	}
	for _, digest := range transcriptDigests(input.Events) {
		content, ok := ablated.SpanData[digest]
		if !ok {
			continue
		}
		var diff evidenceChange
		switch level {
		case fidelityOutcomeAblated:
			content, diff = stripOutcomes(content)
		case fidelityResultsTruncated:
			content, diff = truncateAtFirstResult(content)
		case fidelityTranscriptMissing:
			delete(ablated.SpanData, digest)
			diff = evidenceChange{TranscriptsWithheld: 1}
		}
		if !diff.any() {
			continue
		}
		if level != fidelityTranscriptMissing {
			ablated.SpanData[digest] = content
		}
		change.TranscriptsAffected++
		change.OutcomesRemoved += diff.OutcomesRemoved
		change.RecordsDropped += diff.RecordsDropped
		change.TranscriptsWithheld += diff.TranscriptsWithheld
	}
	return ablated, change
}

// transcriptDigests are the content digests of the run's recorded canonical
// transcripts, sorted and deduplicated.
func transcriptDigests(events []journal.Event) []string {
	seen := map[string]bool{}
	var digests []string
	for _, event := range events {
		if !isTranscriptSpan(event) || seen[event.Ref.Digest] {
			continue
		}
		seen[event.Ref.Digest] = true
		digests = append(digests, event.Ref.Digest)
	}
	sort.Strings(digests)
	return digests
}

func isTranscriptSpan(event journal.Event) bool {
	return event.Type == journal.EventSpanRecorded && event.DataSchema == telemetry.GenAIEventSchema &&
		event.Ref != nil && event.Ref.Digest != ""
}

// stripOutcomes removes the success field from every tool result record and
// leaves every other line byte-for-byte as recorded.
func stripOutcomes(content string) (string, evidenceChange) {
	lines := strings.Split(content, "\n")
	var change evidenceChange
	for index, line := range lines {
		stripped, ok := stripOutcome(line)
		if ok {
			lines[index] = stripped
			change.OutcomesRemoved++
		}
	}
	return strings.Join(lines, "\n"), change
}

func stripOutcome(line string) (string, bool) {
	record, ok := decodeRecord(line)
	if !ok || !record.isToolResult() || record.ToolCall.Success == nil {
		return line, false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(line), &fields) != nil {
		return line, false
	}
	var call map[string]json.RawMessage
	if json.Unmarshal(fields["tool_call"], &call) != nil {
		return line, false
	}
	delete(call, "success")
	encodedCall, err := json.Marshal(call)
	if err != nil {
		return line, false
	}
	fields["tool_call"] = encodedCall
	encoded, err := json.Marshal(fields)
	if err != nil {
		return line, false
	}
	return string(encoded), true
}

// truncateAtFirstResult keeps the records before the transcript's first tool
// result, drops it and everything after it, and appends truncationMarker. A
// transcript with no tool result is unchanged.
func truncateAtFirstResult(content string) (string, evidenceChange) {
	lines := strings.Split(content, "\n")
	for index, line := range lines {
		record, ok := decodeRecord(line)
		if !ok || !record.isToolResult() {
			continue
		}
		dropped := 0
		for _, rest := range lines[index:] {
			if strings.TrimSpace(rest) != "" {
				dropped++
			}
		}
		kept := append(append([]string(nil), lines[:index]...), truncationMarker, "")
		return strings.Join(kept, "\n"), evidenceChange{RecordsDropped: dropped}
	}
	return content, evidenceChange{}
}

func decodeRecord(line string) (transcriptRecord, bool) {
	trimmed := bytes.TrimSpace([]byte(line))
	if len(trimmed) == 0 {
		return transcriptRecord{}, false
	}
	var record transcriptRecord
	if json.Unmarshal(trimmed, &record) != nil {
		return transcriptRecord{}, false
	}
	return record, true
}

// adapterCoverage counts the tool evidence one adapter's transcripts carry at
// one fidelity level.
type adapterCoverage struct {
	Adapter                string `json:"adapter"`
	Cases                  int    `json:"cases"`
	Transcripts            int    `json:"transcripts"`
	TranscriptsWithContent int    `json:"transcriptsWithContent"`
	ToolCalls              int    `json:"toolCalls"`
	ToolResults            int    `json:"toolResults"`
	ToolResultsWithOutcome int    `json:"toolResultsWithOutcome"`
	TruncatedRecords       int    `json:"truncatedRecords"`
}

// coverageCollector accumulates adapter coverage across cases.
type coverageCollector struct {
	byAdapter              map[string]*adapterCoverage
	casesWithoutTranscript int
}

func newCoverageCollector() *coverageCollector {
	return &coverageCollector{byAdapter: map[string]*adapterCoverage{}}
}

// add records one (possibly ablated) case and returns the adapters its
// transcripts came from, sorted.
func (collector *coverageCollector) add(input evalInput) []string {
	caseAdapters := map[string]bool{}
	seen := map[string]bool{}
	for _, event := range input.Events {
		if !isTranscriptSpan(event) || seen[event.Ref.Digest] {
			continue
		}
		seen[event.Ref.Digest] = true
		adapter := adapterOf(event)
		coverage := collector.entry(adapter)
		if !caseAdapters[adapter] {
			caseAdapters[adapter] = true
			coverage.Cases++
		}
		coverage.Transcripts++
		if content, ok := input.SpanData[event.Ref.Digest]; ok {
			coverage.TranscriptsWithContent++
			coverage.countRecords(content)
		}
	}
	if len(caseAdapters) == 0 {
		collector.casesWithoutTranscript++
	}
	adapters := make([]string, 0, len(caseAdapters))
	for adapter := range caseAdapters {
		adapters = append(adapters, adapter)
	}
	sort.Strings(adapters)
	return adapters
}

func (collector *coverageCollector) entry(adapter string) *adapterCoverage {
	coverage, ok := collector.byAdapter[adapter]
	if !ok {
		coverage = &adapterCoverage{Adapter: adapter}
		collector.byAdapter[adapter] = coverage
	}
	return coverage
}

func (collector *coverageCollector) coverage() []adapterCoverage {
	entries := make([]adapterCoverage, 0, len(collector.byAdapter))
	for _, coverage := range collector.byAdapter {
		entries = append(entries, *coverage)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Adapter < entries[j].Adapter })
	return entries
}

func (coverage *adapterCoverage) countRecords(content string) {
	for _, line := range strings.Split(content, "\n") {
		record, ok := decodeRecord(line)
		if !ok {
			continue
		}
		if record.Truncated {
			coverage.TruncatedRecords++
		}
		switch {
		case record.isToolResult():
			coverage.ToolResults++
			if record.ToolCall.Success != nil {
				coverage.ToolResultsWithOutcome++
			}
		case record.ToolCall != nil:
			coverage.ToolCalls++
		}
	}
}

// adapterOf names the adapter a transcript span came from. The harness names
// its transcript span "<adapter>.transcript"; a span named after its stage,
// or any other name, does not identify the adapter.
func adapterOf(event journal.Event) string {
	adapter, ok := strings.CutSuffix(event.Name, ".transcript")
	if !ok || adapter == "" || adapter == event.Stage {
		return unidentifiedAdapter
	}
	return adapter
}
