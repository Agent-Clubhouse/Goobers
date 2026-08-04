package mcpio

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
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

// resolveInWorkspace joins rel onto the workspace root and verifies the
// result cannot escape it, lexically or via a symlinked ancestor — the same
// containment discipline internal/harness's InputArtifactFile lift and
// internal/sandbox use elsewhere, adapted to not require the leaf to already
// exist (publish_output creates a new file; ResolveContainedPath's
// EvalSymlinks on the full path would reject that).
//
// A naive "EvalSymlinks the immediate parent, ignore the error if it
// doesn't exist yet" check (the original version of this function) is
// exploitable: for an artifact like "link/new/out.md" where link points
// outside the workspace and "new" doesn't exist yet, EvalSymlinks(dir)
// fails closed-looking but open — the error was silently ignored — and a
// subsequent os.MkdirAll would then walk through "link" (MkdirAll follows
// symlinks at existing intermediate components, same as any normal path
// resolution) and create "new" outside the workspace. Instead: walk up from
// the leaf's directory to the nearest ancestor that actually exists,
// EvalSymlinks *that* (it's guaranteed to exist, so this can't silently
// no-op), recheck containment on the result, and only then create the
// missing intermediate components — one at a time with os.Mkdir, never
// os.MkdirAll, so nothing created here can itself be, or traverse, a
// symlink; os.Mkdir fails closed (EEXIST) rather than following anything
// planted at that path between the walk-up and the create.
func (t *Toolset) resolveInWorkspace(rel string, createMissingDirs bool) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("empty path")
	}
	root, err := filepath.EvalSymlinks(t.cfg.Workspace)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	full := filepath.Join(root, rel)
	relBack, err := filepath.Rel(root, full)
	if err != nil || relBack == ".." || strings.HasPrefix(relBack, ".."+string(filepath.Separator)) || filepath.IsAbs(relBack) {
		return "", fmt.Errorf("path %q escapes the workspace", rel)
	}

	// Walk up from the leaf's directory to the nearest ancestor that
	// actually exists — an intermediate component further down may not
	// exist yet, and EvalSymlinks requires its argument to exist.
	existing := filepath.Dir(full)
	var missing []string // deepest-first
	for existing != root {
		if _, err := os.Lstat(existing); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("path %q: %w", rel, err)
		}
		missing = append(missing, filepath.Base(existing))
		existing = filepath.Dir(existing)
	}

	resolvedExisting, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", rel, err)
	}
	relExisting, err := filepath.Rel(root, resolvedExisting)
	if err != nil || relExisting == ".." || strings.HasPrefix(relExisting, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q's directory escapes the workspace", rel)
	}

	dir := resolvedExisting
	if len(missing) > 0 {
		if !createMissingDirs {
			return "", fmt.Errorf("path %q: no such directory", rel)
		}
		for i := len(missing) - 1; i >= 0; i-- {
			dir = filepath.Join(dir, missing[i])
			if err := os.Mkdir(dir, 0o755); err != nil {
				return "", fmt.Errorf("create directory for %q: %w", rel, err)
			}
		}
	}

	full = filepath.Join(dir, filepath.Base(full))
	// The leaf itself may already exist as a symlink to somewhere outside
	// the workspace — lexically "full" is still inside root, so the checks
	// above pass, but os.WriteFile/os.ReadFile follow a symlink at open()
	// time and would write or read through it to wherever it actually
	// points. Lstat (not Stat) so this inspects the link itself, not its
	// target; any existing symlink at this exact path is rejected outright
	// rather than resolved-and-rechecked — no legitimate caller of this
	// package ever needs to write or read through a pre-existing symlink at
	// the leaf position.
	if info, err := os.Lstat(full); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("path %q is a symlink; refusing to read or write through it", rel)
	}
	return full, nil
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
