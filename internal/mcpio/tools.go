package mcpio

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// defaultReadLineCap bounds a range-less read_input call — large enough for
// the findings/report-shaped content this exists for, small enough that a
// genuinely large input comes back explicitly truncated (Truncated: true,
// TotalLines reported) rather than silently blowing the caller's context
// budget.
const defaultReadLineCap = 2000

// maxGrepMatches bounds a single grep_input call the same way: a
// pathological match count comes back explicitly truncated, never silently
// dropped past this point.
const maxGrepMatches = 200

// Toolset implements the generic tools against a fixed Config, loaded once
// at process start. It never re-reads Config or talks to the journal —
// every call is a plain, local filesystem operation scoped to Workspace.
type Toolset struct {
	cfg Config
}

// NewToolset builds a Toolset from an already-loaded, already-validated
// Config.
func NewToolset(cfg Config) *Toolset {
	return &Toolset{cfg: cfg}
}

// RunInfo is get_run_info's return shape.
type RunInfo struct {
	RunID      string `json:"runId"`
	WorkflowID string `json:"workflowId"`
	TaskID     string `json:"taskId"`
	Gaggle     string `json:"gaggle"`
}

// GetRunInfo returns the stage identity captured in the invocation envelope.
func (t *Toolset) GetRunInfo() RunInfo {
	return RunInfo{
		RunID:      t.cfg.RunID,
		WorkflowID: t.cfg.WorkflowID,
		TaskID:     t.cfg.TaskID,
		Gaggle:     t.cfg.Gaggle,
	}
}

// resolveInWorkspace resolves rel against the workspace root using the same
// no-follow-anywhere-in-the-chain discipline as resolveRooted (see its doc
// comment) — this is just that logic scoped to t.cfg.Workspace.
func (t *Toolset) resolveInWorkspace(rel string, createMissingDirs bool) (string, error) {
	return resolveRooted(t.cfg.Workspace, rel, createMissingDirs)
}

// PublishOutput writes content to the task's declared artifactFile. There is
// exactly one such target per invocation today (a task has one artifactFile
// input, not a map) — see #2406's issue body for why this doesn't take a
// name parameter yet.
func (t *Toolset) PublishOutput(content string) (bytesWritten int, err error) {
	if t.cfg.ArtifactFile == "" {
		return 0, fmt.Errorf("this stage declares no artifactFile input — publish_output has nothing to write to")
	}
	full, err := t.resolveInWorkspace(t.cfg.ArtifactFile, true)
	if err != nil {
		return 0, fmt.Errorf("resolve artifactFile: %w", err)
	}
	data := []byte(content)
	if err := os.WriteFile(full, data, 0o644); err != nil {
		return 0, fmt.Errorf("write artifactFile: %w", err)
	}
	return len(data), nil
}

// InputSummary is one entry list_inputs returns.
type InputSummary struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"sizeBytes"`
	LineCount int    `json:"lineCount"`
}

// ListInputs enumerates every upstream artifact already materialized into
// this stage's workspace (Config.Inputs, populated by materializeContext —
// see internal/harness/context.go), sorted by name for deterministic output.
// LineCount is reported alongside SizeBytes because read_input/grep_input
// are line-addressed, not byte-addressed — it's the number that actually
// tells a caller whether an input is worth paginating.
func (t *Toolset) ListInputs() ([]InputSummary, error) {
	names := make([]string, 0, len(t.cfg.Inputs))
	for name := range t.cfg.Inputs {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]InputSummary, 0, len(names))
	for _, name := range names {
		data, err := t.readInputFile(name)
		if err != nil {
			return nil, err
		}
		out = append(out, InputSummary{
			Name:      name,
			SizeBytes: int64(len(data)),
			LineCount: len(splitLines(data)),
		})
	}
	return out, nil
}

func (t *Toolset) readInputFile(name string) ([]byte, error) {
	rel, ok := t.cfg.Inputs[name]
	if !ok {
		return nil, fmt.Errorf("no input named %q — call list_inputs first", name)
	}
	full, err := t.resolveInWorkspace(rel, false)
	if err != nil {
		return nil, fmt.Errorf("input %q: %w", name, err)
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return nil, fmt.Errorf("read input %q: %w", name, err)
	}
	return data, nil
}

func (t *Toolset) inputDigest(name string) (string, error) {
	data, err := t.readInputFile(name)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(data)), nil
}

// splitLines splits data on newlines with a trailing newline (if any)
// ignored, so a file ending "a\nb\nc\n" reports 3 lines, not 4, and an empty
// file reports 0.
func splitLines(data []byte) []string {
	trimmed := bytes.TrimRight(data, "\n")
	if len(trimmed) == 0 {
		return nil
	}
	return strings.Split(string(trimmed), "\n")
}

// ReadResult is read_input's return shape.
type ReadResult struct {
	Content    string `json:"content"`
	StartLine  int    `json:"startLine"`
	EndLine    int    `json:"endLine"`
	TotalLines int    `json:"totalLines"`
	Truncated  bool   `json:"truncated"`
}

// ReadInput returns lines [startLine, endLine] (1-based, inclusive) of a
// named input. startLine/endLine <= 0 means "unspecified": with both
// unspecified, the whole input is returned unless it exceeds
// defaultReadLineCap, in which case the result is explicitly truncated
// (Truncated: true, TotalLines still reported) rather than silently cut off.
func (t *Toolset) ReadInput(name string, startLine, endLine int) (ReadResult, error) {
	data, err := t.readInputFile(name)
	if err != nil {
		return ReadResult{}, err
	}
	lines := splitLines(data)
	total := len(lines)

	start, end := startLine, endLine
	truncated := false
	switch {
	case start <= 0 && end <= 0:
		start, end = 1, total
		if total > defaultReadLineCap {
			end = defaultReadLineCap
			truncated = true
		}
	default:
		if start <= 0 {
			start = 1
		}
		if end <= 0 || end > total {
			end = total
		}
	}
	if start > total {
		return ReadResult{StartLine: start, EndLine: start - 1, TotalLines: total}, nil
	}
	if end < start {
		end = start
	}
	return ReadResult{
		Content:    strings.Join(lines[start-1:end], "\n"),
		StartLine:  start,
		EndLine:    end,
		TotalLines: total,
		Truncated:  truncated,
	}, nil
}

// GrepMatch is one match grep_input found.
type GrepMatch struct {
	LineNumber   int    `json:"lineNumber"`
	Line         string `json:"line"`
	ContextStart int    `json:"contextStart"`
	ContextEnd   int    `json:"contextEnd"`
}

// GrepResult is grep_input's return shape.
type GrepResult struct {
	Matches   []GrepMatch `json:"matches"`
	Truncated bool        `json:"truncated"`
}

// GrepInput searches a named input for a regular expression, returning each
// matching line's number and a context window (ContextStart/ContextEnd)
// sized by contextLines — meant to be passed straight to a follow-up
// ReadInput(name, contextStart, contextEnd) call. Match count is capped at
// maxGrepMatches, explicitly (Truncated: true), never silently.
func (t *Toolset) GrepInput(name, pattern string, contextLines int) (GrepResult, error) {
	data, err := t.readInputFile(name)
	if err != nil {
		return GrepResult{}, err
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return GrepResult{}, fmt.Errorf("invalid pattern %q: %w", pattern, err)
	}
	if contextLines < 0 {
		contextLines = 0
	}
	lines := splitLines(data)
	result := GrepResult{}
	for i, line := range lines {
		if !re.MatchString(line) {
			continue
		}
		if len(result.Matches) >= maxGrepMatches {
			result.Truncated = true
			break
		}
		lineNumber := i + 1
		start := lineNumber - contextLines
		if start < 1 {
			start = 1
		}
		end := lineNumber + contextLines
		if end > len(lines) {
			end = len(lines)
		}
		result.Matches = append(result.Matches, GrepMatch{
			LineNumber:   lineNumber,
			Line:         line,
			ContextStart: start,
			ContextEnd:   end,
		})
	}
	return result, nil
}
