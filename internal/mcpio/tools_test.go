package mcpio

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func newTestToolset(t *testing.T, files map[string]string) (*Toolset, string) {
	t.Helper()
	ws := t.TempDir()
	inputs := map[string]string{}
	for name, content := range files {
		rel := filepath.Join(".goobers", "context", name+".txt")
		full := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		inputs[name] = rel
	}
	return NewToolset(Config{Workspace: ws, ArtifactFile: "out.md", Inputs: inputs}), ws
}

func TestGetRunInfo(t *testing.T) {
	tool := NewToolset(Config{
		RunID:      "run-123",
		WorkflowID: "implementation",
		TaskID:     "implement",
		Gaggle:     "goobers",
	})
	got := tool.GetRunInfo()
	want := RunInfo{
		RunID:      "run-123",
		WorkflowID: "implementation",
		TaskID:     "implement",
		Gaggle:     "goobers",
	}
	if got != want {
		t.Fatalf("GetRunInfo() = %+v, want %+v", got, want)
	}
}

func TestListInputsReportsLineCount(t *testing.T) {
	tool, _ := newTestToolset(t, map[string]string{
		"three-lines":  "a\nb\nc\n",
		"no-trailing":  "a\nb\nc",
		"empty":        "",
		"single-blank": "\n",
	})
	items, err := tool.ListInputs()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, item := range items {
		got[item.Name] = item.LineCount
	}
	want := map[string]int{"three-lines": 3, "no-trailing": 3, "empty": 0, "single-blank": 0}
	for name, wantLines := range want {
		if got[name] != wantLines {
			t.Errorf("%s: LineCount = %d, want %d", name, got[name], wantLines)
		}
	}
}

func TestReadInputWholeFileUnderCap(t *testing.T) {
	tool, _ := newTestToolset(t, map[string]string{"f": "a\nb\nc\n"})
	result, err := tool.ReadInput("f", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "a\nb\nc" || result.StartLine != 1 || result.EndLine != 3 ||
		result.TotalLines != 3 || result.Truncated {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestReadInputExplicitRange(t *testing.T) {
	tool, _ := newTestToolset(t, map[string]string{"f": "1\n2\n3\n4\n5\n"})
	result, err := tool.ReadInput("f", 2, 4)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "2\n3\n4" || result.StartLine != 2 || result.EndLine != 4 || result.TotalLines != 5 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestReadInputTruncatesLargeFileByDefault(t *testing.T) {
	lines := make([]string, defaultReadLineCap+50)
	for i := range lines {
		lines[i] = strconv.Itoa(i + 1)
	}
	tool, _ := newTestToolset(t, map[string]string{"big": strings.Join(lines, "\n") + "\n"})

	result, err := tool.ReadInput("big", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Truncated {
		t.Fatal("expected Truncated=true for a file over the default cap")
	}
	if result.EndLine != defaultReadLineCap {
		t.Errorf("EndLine = %d, want %d", result.EndLine, defaultReadLineCap)
	}
	if result.TotalLines != len(lines) {
		t.Errorf("TotalLines = %d, want %d", result.TotalLines, len(lines))
	}

	// An explicit range past the cap must still work — truncation only
	// applies to the "give me everything" case.
	full, err := tool.ReadInput("big", 1, len(lines))
	if err != nil {
		t.Fatal(err)
	}
	if full.Truncated {
		t.Fatal("an explicit full range must not be reported truncated")
	}
	if full.EndLine != len(lines) {
		t.Errorf("EndLine = %d, want %d", full.EndLine, len(lines))
	}
}

func TestReadInputStartPastEndOfFile(t *testing.T) {
	tool, _ := newTestToolset(t, map[string]string{"f": "a\nb\n"})
	result, err := tool.ReadInput("f", 10, 20)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "" || result.TotalLines != 2 {
		t.Fatalf("unexpected result for out-of-range start: %+v", result)
	}
}

func TestGrepInputFindsMatchesWithContext(t *testing.T) {
	tool, _ := newTestToolset(t, map[string]string{
		"f": "one\ntwo needle\nthree\nfour needle\nfive\n",
	})
	result, err := tool.GrepInput("f", "needle", 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Truncated {
		t.Fatal("did not expect truncation")
	}
	if len(result.Matches) != 2 {
		t.Fatalf("got %d matches, want 2: %+v", len(result.Matches), result.Matches)
	}
	first := result.Matches[0]
	if first.LineNumber != 2 || first.Line != "two needle" || first.ContextStart != 1 || first.ContextEnd != 3 {
		t.Errorf("first match = %+v", first)
	}
	second := result.Matches[1]
	if second.LineNumber != 4 || second.ContextStart != 3 || second.ContextEnd != 5 {
		t.Errorf("second match = %+v", second)
	}
}

func TestGrepInputTruncatesAtMaxMatches(t *testing.T) {
	lines := make([]string, maxGrepMatches+10)
	for i := range lines {
		lines[i] = "needle"
	}
	tool, _ := newTestToolset(t, map[string]string{"f": strings.Join(lines, "\n")})
	result, err := tool.GrepInput("f", "needle", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Truncated {
		t.Fatal("expected Truncated=true past maxGrepMatches")
	}
	if len(result.Matches) != maxGrepMatches {
		t.Fatalf("got %d matches, want exactly the cap %d", len(result.Matches), maxGrepMatches)
	}
}

func TestGrepInputRejectsInvalidPattern(t *testing.T) {
	tool, _ := newTestToolset(t, map[string]string{"f": "x"})
	if _, err := tool.GrepInput("f", "(unclosed", 0); err == nil {
		t.Fatal("expected an error for an invalid regex")
	}
}

func TestGrepInputUnknownInput(t *testing.T) {
	tool, _ := newTestToolset(t, map[string]string{"f": "x"})
	if _, err := tool.GrepInput("missing", "x", 0); err == nil {
		t.Fatal("expected an error for an unknown input name")
	}
}
