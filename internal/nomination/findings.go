package nomination

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Tool names a deterministic tool whose findings the filer can confirm
// (engagement decision 004): go vet, golangci-lint, and go test. Each is run
// by the collect-repo-signals stage over the repository at the run's base
// revision, and its raw output is recorded as that stage's stdout artifact.
type Tool string

// Tools.
const (
	ToolVet  Tool = "vet"
	ToolLint Tool = "lint"
	ToolTest Tool = "test"
)

// Finding is one diagnostic exactly as a tool recorded it. The identity the
// filer matches byte for byte is the whole tuple:
//
//   - vet:  Path, Line and Rule — the diagnostic text after the position,
//     since go vet names no analyzer in its default output;
//   - lint: Path, Line and Rule — the golangci-lint issue's FromLinter;
//   - test: Package and Test — a test2json fail event with a test name
//     (a package-level fail with no test is a go-package signal, not a
//     finding a nomination can match).
type Finding struct {
	Tool    Tool
	Rule    string
	Path    string
	Line    int
	Package string
	Test    string
}

// key is the exact-match identity: every field, byte for byte.
func (f Finding) key() string {
	return strings.Join([]string{string(f.Tool), f.Rule, f.Path, strconv.Itoa(f.Line), f.Package, f.Test}, "\x00")
}

// String renders a finding the way the issue body and the unmet-approval
// reasons name it.
func (f Finding) String() string {
	switch f.Tool {
	case ToolTest:
		return fmt.Sprintf("test %s %s", f.Package, f.Test)
	default:
		return fmt.Sprintf("%s %s:%d %s", f.Tool, f.Path, f.Line, f.Rule)
	}
}

// Findings is the set of findings parsed out of one collect-repo-signals
// stdout artifact. It is built from the tool output alone; nothing the
// nominating model writes reaches it.
type Findings struct {
	byKey map[string]Finding
	// Counts is the number of findings parsed per tool.
	Counts map[Tool]int
	// Problems names sections the parser could not read completely (a
	// truncated golangci-lint JSON document, an unparseable test2json line).
	// A problem loses findings, never invents them: a nomination naming a
	// lost finding files unapproved.
	Problems []string
}

// Section headers the collect-repo-signals stage prints around each tool's
// raw output: "=== <name> ===" or "=== <name> (exit <n>) ===".
var (
	sectionHeader = regexp.MustCompile(`^=== (.+?)(?: \(exit -?\d+\))? ===$`)
	// vetDiagnostic is one go vet line: "<file>.go:<line>:<col>: <text>". A
	// leading "./" (go vet's shape when run inside a package directory) is
	// trimmed so the path compares to the clean relative path the artifact
	// validator admits.
	vetDiagnostic = regexp.MustCompile(`^(?:\./)?([^\s:]+\.go):(\d+):(\d+): (.+)$`)
)

const (
	sectionVet  = "go vet"
	sectionLint = "golangci-lint"
	sectionTest = "go test failures"
)

// ParseSignals parses the raw collect-repo-signals stdout: the go vet
// diagnostics, the golangci-lint JSON issues, and the go test -json fail
// events, each under its fixed section header. Unknown sections are ignored.
func ParseSignals(stdout []byte) *Findings {
	f := &Findings{byKey: map[string]Finding{}, Counts: map[Tool]int{}}
	sections, err := splitSections(stdout)
	if err != nil {
		f.Problems = append(f.Problems, "stdout could not be read to the end: "+err.Error())
	}
	for _, line := range strings.Split(sections[sectionVet], "\n") {
		m := vetDiagnostic.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		lineNo, err := strconv.Atoi(m[2])
		if err != nil || lineNo <= 0 {
			continue
		}
		f.add(Finding{Tool: ToolVet, Path: m[1], Line: lineNo, Rule: m[4]})
	}
	if body, ok := sections[sectionLint]; ok {
		f.parseLint(body)
	}
	for _, line := range strings.Split(sections[sectionTest], "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var event struct {
			Action  string `json:"Action"`
			Package string `json:"Package"`
			Test    string `json:"Test"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			f.Problems = append(f.Problems, fmt.Sprintf("%s: unparseable test2json line (truncated?): %.60q", sectionTest, line))
			continue
		}
		if event.Action != "fail" || event.Test == "" || event.Package == "" {
			continue
		}
		f.add(Finding{Tool: ToolTest, Package: event.Package, Test: event.Test})
	}
	return f
}

// splitSections cuts stdout at its section headers and returns each named
// section's body (the text between its header and the next). A line the
// scanner cannot buffer ends the scan; what was read stands and the error
// is reported as a parse problem.
func splitSections(stdout []byte) (map[string]string, error) {
	sections := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	current := ""
	var body strings.Builder
	flush := func() {
		if current != "" {
			sections[current] = body.String()
		}
		body.Reset()
	}
	for scanner.Scan() {
		line := scanner.Text()
		if m := sectionHeader.FindStringSubmatch(strings.TrimRight(line, "\r")); m != nil {
			flush()
			current = m[1]
			continue
		}
		if current != "" {
			body.WriteString(line)
			body.WriteByte('\n')
		}
	}
	flush()
	return sections, scanner.Err()
}

// parseLint decodes the golangci-lint JSON document at the head of its
// section (the stage prints lint.json, then the linter's stderr) — one JSON
// value, decoded from the first "{"; a truncated document is a problem, not
// a partial set of findings.
func (f *Findings) parseLint(body string) {
	start := strings.IndexByte(body, '{')
	if start < 0 {
		f.Problems = append(f.Problems, sectionLint+": no JSON document in the section")
		return
	}
	var report struct {
		Issues []struct {
			FromLinter string `json:"FromLinter"`
			Pos        struct {
				Filename string `json:"Filename"`
				Line     int    `json:"Line"`
			} `json:"Pos"`
		} `json:"Issues"`
	}
	if err := json.NewDecoder(strings.NewReader(body[start:])).Decode(&report); err != nil {
		f.Problems = append(f.Problems, fmt.Sprintf("%s: JSON document is not parseable (truncated?): %v", sectionLint, err))
		return
	}
	for _, issue := range report.Issues {
		if issue.FromLinter == "" || issue.Pos.Filename == "" || issue.Pos.Line <= 0 {
			continue
		}
		f.add(Finding{Tool: ToolLint, Rule: issue.FromLinter, Path: strings.TrimPrefix(issue.Pos.Filename, "./"), Line: issue.Pos.Line})
	}
}

func (f *Findings) add(finding Finding) {
	key := finding.key()
	if _, dup := f.byKey[key]; dup {
		return
	}
	f.byKey[key] = finding
	f.Counts[finding.Tool]++
}

// Match reports the tool finding an evidence pointer of kind finding names,
// when the artifact contains it byte for byte. Any other evidence kind, and
// any difference in tool, rule, path, line, package or test, is no match.
func (f *Findings) Match(e Evidence) (Finding, bool) {
	if f == nil || e.Kind != EvidenceFinding {
		return Finding{}, false
	}
	want := Finding{Tool: e.Tool, Rule: e.Rule, Path: e.Path, Line: e.Line, Package: e.Package, Test: e.Test}
	got, ok := f.byKey[want.key()]
	return got, ok
}
