package executor

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

const maxFailureSummaryBytes = 512

const (
	outputFailureArtifact  = "failureArtifact"
	outputFailureStartByte = "failureStartByte"
	outputFailureEndByte   = "failureEndByte"
	outputWarningArtifact  = "warningArtifact"
	outputWarningStartByte = "warningStartByte"
	outputWarningEndByte   = "warningEndByte"
)

var (
	testFailurePattern   = regexp.MustCompile(`(?i)^(?:FAIL\s+.+|--- FAIL:\s*.+|Failed\s+.+|\S.*\s[>›]\s.+)$`)
	compilerErrorPattern = regexp.MustCompile(`(?i)(?:^|[\s:])(?:fatal\s+)?error(?:\[[A-Z0-9]+\]|\s+[A-Z]+\d+)?:|(?:^|\s)\S+\.go:\d+(?::\d+)?:\s+(?:undefined:|cannot |assignment mismatch|declared and not used|imported and not used|invalid operation|not enough arguments|too many arguments|syntax error:)`)
	buildFailurePattern  = regexp.MustCompile(`(?i)(?:make(?:\[\d+\])?: \*\*\*|Execution failed for task\b|error: command failed|\[ERROR\].*failed to execute goal)`)
	buildSummaryPattern  = regexp.MustCompile(`(?i)(?:BUILD FAILED|FAILURE: Build failed|ninja: build stopped|npm error|ELIFECYCLE)`)
	warningPattern       = regexp.MustCompile(`(?i)(?:^|[\s:])warn(?:ing)?(?:\s|:|\[)`)
	assertionPattern     = regexp.MustCompile(`(?i)(?:AssertionError|Error Message:|Assert\.|Expected:|Received:|expected .+ (?:to|but)|panic:|\.go:\d+|error\s+[A-Z]+\d+:)`)
	ansiPattern          = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
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
			candidate := diagnosticRange{
				text:     failureSection(lines, i),
				stream:   stream.name,
				start:    line.start,
				end:      failureSectionEnd(lines, i),
				priority: priority,
			}
			if priority >= best.priority {
				best = candidate
			}
		}
	}

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
	return commandFailureDiagnostic{failure: best, warning: warning}
}

func failureLineSpecificity(line string) int {
	line = cleanOutputLine(line)
	switch {
	case testFailurePattern.MatchString(line) &&
		!strings.HasPrefix(strings.ToLower(line), "failed tests") &&
		!strings.HasPrefix(line, "FAIL\t"):
		return 2
	case compilerErrorPattern.MatchString(line):
		return 2
	case buildFailurePattern.MatchString(line):
		return 2
	case buildSummaryPattern.MatchString(line):
		return 1
	default:
		return 0
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
	result.Error.Message = message
	result.Summary = message
	return true
}
