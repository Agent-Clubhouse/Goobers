package executor

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/failureclass"
)

const maxFailureSummaryBytes = 512

// maxFailureDigestBytes bounds the FULL roster of failures handed to a repass.
// The one-line summary above stays short for journals and status output; this
// is the evidence an implementer actually has to act on, so it is sized to
// hold every failing test in a realistic suite rather than the first one
// (#5101: successive repasses each saw a single test, fixed it, paid for
// another full `make ci`, and met the next — exhausting the budget on a sound
// change).
const maxFailureDigestBytes = 8192

const (
	outputFailureArtifact  = "failureArtifact"
	outputFailureStartByte = "failureStartByte"
	outputFailureEndByte   = "failureEndByte"
	outputWarningArtifact  = "warningArtifact"
	outputWarningStartByte = "warningStartByte"
	outputWarningEndByte   = "warningEndByte"
	// The full roster (#5101). Separate from failureArtifact, which stays a
	// pointer to one byte range in one stream.
	outputFailureDigest = "failureDigest"
	outputFailureCount  = "failureCount"
)

var (
	testFailurePattern   = regexp.MustCompile(`(?i)^(?:FAIL\s+.+|--- FAIL:\s*.+|Failed\s+.+|\S.*\s[>›]\s.+)$`)
	compilerErrorPattern = regexp.MustCompile(`(?i)(?:^|[\s:])(?:fatal\s+)?error(?:\[[A-Z0-9]+\]|\s+[A-Z]+\d+)?:|(?:^|\s)\S+\.go:\d+(?::\d+)?:\s+(?:undefined:|cannot |assignment mismatch|declared and not used|imported and not used|invalid operation|not enough arguments|too many arguments|syntax error:)`)
	buildFailurePattern  = regexp.MustCompile(`(?i)(?:make(?:\[\d+\])?: \*\*\*|Execution failed for task\b|error: command failed|\[ERROR\].*failed to execute goal)`)
	buildSummaryPattern  = regexp.MustCompile(`(?i)(?:BUILD FAILED|FAILURE: Build failed|ninja: build stopped|npm error|ELIFECYCLE)`)
	warningPattern       = regexp.MustCompile(`(?i)(?:^|[\s:])warn(?:ing)?(?:\s|:|\[)`)
	assertionPattern     = regexp.MustCompile(`(?i)(?:AssertionError|Error Message:|Assert\.|Expected:|Received:|expected .+ (?:to|but)|panic:|\.go:\d+|error\s+[A-Z]+\d+:)`)
	ansiPattern          = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
	// "FAIL\tpkg\t1315.643s" / "FAIL\tpkg [build failed]" — go test's
	// package-level verdict, which names no individual test.
	packageFailurePattern = regexp.MustCompile(`^FAIL\s+\S+`)
)

type outputLine struct {
	text       string
	start, end int
}

type diagnosticRange struct {
	text       string
	stream     string
	start, end int
	priority   int
}

type commandFailureDiagnostic struct {
	failure diagnosticRange
	warning diagnosticRange
	// digest lists EVERY distinct failure line found, most specific first,
	// deduplicated and bounded. failure above remains the single best window
	// for the short summary and the byte-range artifact pointer.
	digest []string
}

func summarizeCommandFailure(stdout, stderr []byte) commandFailureDiagnostic {
	var best diagnosticRange
	for _, stream := range []struct {
		name string
		data []byte
	}{
		{name: "stdout", data: stdout},
		{name: "stderr", data: stderr},
	} {
		lines := splitOutputLines(stream.data)
		for i, line := range lines {
			priority := failureLineSpecificity(line.text)
			if priority == 0 || priority < best.priority {
				continue
			}
			if priority == best.priority && stream.name == "stderr" && best.stream == "stdout" {
				// An equal-priority stderr line (a make trailer, a wrapper's exit-status
				// echo) must not displace a diagnostic already found in stdout: stderr is
				// scanned after stdout, so without this guard trailing noise silently
				// steals the recorded failure window from the real error.
				continue
			}
			best = diagnosticRange{
				text:     failureSection(lines, i),
				stream:   stream.name,
				start:    line.start,
				end:      failureSectionEnd(lines, i),
				priority: priority,
			}
		}
	}

	digest := collectFailureDigest(stdout, stderr)

	var warning diagnosticRange
	for _, stream := range []struct {
		name string
		data []byte
	}{
		{name: "stdout", data: stdout},
		{name: "stderr", data: stderr},
	} {
		for _, line := range splitOutputLines(stream.data) {
			if warningPattern.MatchString(cleanOutputLine(line.text)) &&
				(stream.name != best.stream || line.end <= best.start || line.start >= best.end) {
				warning = diagnosticRange{
					text: cleanOutputLine(line.text), stream: stream.name,
					start: line.start, end: line.end,
				}
			}
		}
	}
	return commandFailureDiagnostic{failure: best, warning: warning, digest: digest}
}

// FailureDiagnostic extracts the failure section a command's output carries —
// the same extraction the shell executor records in a failing stage result's
// summary and error message. It is exported so a caller holding raw command
// output (a baseline probe re-running the identical CI command, #2971) can
// reduce it to exactly the text a stage result already carries, instead of
// comparing an extracted diagnostic against a raw transcript. It returns the
// empty string when the output carries no recognizable failure section.
func FailureDiagnostic(stdout, stderr []byte) string {
	return summarizeCommandFailure(stdout, stderr).failure.text
}

// Specificity tiers are spaced so a tier can be inserted between two existing
// ones without renumbering every caller's expectations. Higher wins.
const (
	specificityNone             = 0
	specificityBuildSummary     = 10
	specificityBuildTrailer     = 15
	specificityPackageFailure   = 20
	specificityTestFailure      = 30
	specificityDependencyDenial = 40
)

// collectFailureDigest returns every distinct failure line across both
// streams, ordered most-specific first and then by appearance, deduplicated
// and bounded by maxFailureDigestBytes.
//
// summarizeCommandFailure deliberately keeps ONE window for the short summary
// and the byte-range pointer. That is right for a journal line and wrong for a
// repass: the implementer needs the whole roster, or it fixes what it can see
// and meets the next failure on the following attempt (#5101).
func collectFailureDigest(stdout, stderr []byte) []string {
	type scored struct {
		text     string
		priority int
		order    int
	}
	var found []scored
	seen := make(map[string]struct{})
	order := 0
	for _, data := range [][]byte{stdout, stderr} {
		for _, line := range splitOutputLines(data) {
			text := cleanOutputLine(line.text)
			priority := failureLineSpecificity(text)
			if priority == specificityNone {
				continue
			}
			if _, duplicate := seen[text]; duplicate {
				continue
			}
			seen[text] = struct{}{}
			found = append(found, scored{text: text, priority: priority, order: order})
			order++
		}
	}
	// Wrapper trailers and build banners ("make: *** [ci] Error 1", "BUILD
	// FAILED") repeat on every failure and name nothing, so they are dropped
	// the moment anything specific exists. They are KEPT when they are all
	// there is: a build that died before any test ran still has to say
	// something, and an empty roster would be a worse lie than a vague one.
	specific := found[:0]
	for _, entry := range found {
		if entry.priority > specificityBuildTrailer {
			specific = append(specific, entry)
		}
	}
	if len(specific) > 0 {
		found = specific
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].priority != found[j].priority {
			return found[i].priority > found[j].priority
		}
		return found[i].order < found[j].order
	})
	var digest []string
	size := 0
	for _, entry := range found {
		if size+len(entry.text)+1 > maxFailureDigestBytes {
			digest = append(digest, fmt.Sprintf("... (%d more failure line(s) omitted at the %d-byte bound)", len(found)-len(digest), maxFailureDigestBytes))
			break
		}
		digest = append(digest, entry.text)
		size += len(entry.text) + 1
	}
	return digest
}

func failureLineSpecificity(line string) int {
	line = cleanOutputLine(line)
	switch {
	// A dependency fetch the network refused is the most specific line a
	// failing build can carry: it names a cause no diff can address. It has
	// to outrank the wrapper trailer below it (#4143 — `make: *** [ci]` is
	// emitted after `npm error 403 Forbidden`, won the tie, and left the
	// recorded diagnostic with no trace of the denial, so the gate's
	// infrastructure classifier could not see one).
	case failureclass.IsDependencyTransportDenial(line):
		return specificityDependencyDenial
	case testFailurePattern.MatchString(line) &&
		!strings.HasPrefix(strings.ToLower(line), "failed tests") &&
		!strings.HasPrefix(line, "FAIL\t"):
		return specificityTestFailure
	case compilerErrorPattern.MatchString(line):
		return specificityTestFailure
	// A package-level "FAIL\tpkg\t12.3s" outranks the wrapper trailer but not
	// a per-test "--- FAIL:". It is excluded from the tier above because it is
	// a SUMMARY of tests reported individually there — but when a package
	// fails WITHOUT naming a test (a killed binary, a coverage-run failure, a
	// build error inside the test binary) it is the only failure signal the
	// output carries, and treating it as nothing sent the diagnostic to
	// stderr's `make: *** [ci] Error 1` instead.
	//
	// MEASURED on the goobernetes cloud instance (#5101, run
	// ebd455dedd8f54270f9e0eb16c462a9c): 55,805 bytes of stdout whose only
	// failure line was "FAIL\tgithub.com/goobers/goobers/cmd/goobers\t1315.643s"
	// — no "--- FAIL:", no panic, no timeout. The implementer was handed
	// stderr's 3,931 bytes of module-download noise and the make trailer, and
	// repassed blind until its budget was exhausted.
	case packageFailurePattern.MatchString(line):
		return specificityPackageFailure
	case buildFailurePattern.MatchString(line):
		return specificityBuildTrailer
	case buildSummaryPattern.MatchString(line):
		return specificityBuildSummary
	default:
		return specificityNone
	}
}

func splitOutputLines(data []byte) []outputLine {
	lines := make([]outputLine, 0, strings.Count(string(data), "\n")+1)
	for start := 0; start < len(data); {
		end := start + bytes.IndexByte(data[start:], '\n')
		if end < start {
			end = len(data)
		} else {
			end++
		}
		lines = append(lines, outputLine{text: string(data[start:end]), start: start, end: end})
		start = end
	}
	return lines
}

func failureSection(lines []outputLine, index int) string {
	selected := []string{cleanOutputLine(lines[index].text)}
	for i := index + 1; i < len(lines) && i <= index+8; i++ {
		line := cleanOutputLine(lines[i].text)
		if line == "" {
			continue
		}
		if assertionPattern.MatchString(line) || strings.HasPrefix(line, ">") {
			selected = append(selected, line)
		}
	}
	return boundDiagnostic(strings.Join(selected, " | "))
}

func failureSectionEnd(lines []outputLine, index int) int {
	end := lines[index].end
	for i := index + 1; i < len(lines) && i <= index+8; i++ {
		line := cleanOutputLine(lines[i].text)
		if assertionPattern.MatchString(line) || strings.HasPrefix(line, ">") {
			end = lines[i].end
		}
	}
	return end
}

func cleanOutputLine(line string) string {
	return strings.TrimSpace(ansiPattern.ReplaceAllString(line, ""))
}

func boundDiagnostic(value string) string {
	if len(value) <= maxFailureSummaryBytes {
		return value
	}
	value = value[:maxFailureSummaryBytes-3]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return strings.TrimSpace(value) + "..."
}

func applyCommandFailureDiagnostic(result *apiv1.ResultEnvelope, exitCode int, diagnostic commandFailureDiagnostic, stdoutPath, stderrPath string) bool {
	if diagnostic.failure.text == "" {
		return false
	}
	path := stdoutPath
	if diagnostic.failure.stream == "stderr" {
		path = stderrPath
	}
	result.Outputs[outputFailureArtifact] = path
	result.Outputs[outputFailureStartByte] = float64(diagnostic.failure.start)
	result.Outputs[outputFailureEndByte] = float64(diagnostic.failure.end)

	message := fmt.Sprintf("command exited %d; failure: %s", exitCode, diagnostic.failure.text)
	if hint := failureclass.DependencyTransportHint(diagnostic.failure.text); hint != "" {
		message += "; hint: " + hint
	}
	if diagnostic.warning.text != "" {
		warningPath := stdoutPath
		if diagnostic.warning.stream == "stderr" {
			warningPath = stderrPath
		}
		result.Outputs[outputWarningArtifact] = warningPath
		result.Outputs[outputWarningStartByte] = float64(diagnostic.warning.start)
		result.Outputs[outputWarningEndByte] = float64(diagnostic.warning.end)
		message += fmt.Sprintf("; warnings: separate %s evidence at bytes %d-%d", warningPath, diagnostic.warning.start, diagnostic.warning.end)
	}
	// The roster goes in Outputs, not in the message: the message is the
	// journal one-liner and status text, and #5101 is precisely that a short
	// message is the ONLY thing a repass ever saw. Outputs travel into the
	// repass context artifact the implementer reads.
	if len(diagnostic.digest) > 1 {
		result.Outputs[outputFailureDigest] = strings.Join(diagnostic.digest, "\n")
		result.Outputs[outputFailureCount] = float64(len(diagnostic.digest))
		message += fmt.Sprintf("; %d distinct failure line(s) recorded in %s", len(diagnostic.digest), outputFailureDigest)
	}
	result.Error.Message = message
	result.Summary = message
	return true
}
