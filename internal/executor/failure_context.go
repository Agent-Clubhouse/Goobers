package executor

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

var failedCISectionPattern = regexp.MustCompile(`^ci: (.+): exit status \d+$`)

// fallbackFailureEvidence treats wrapper-only recognition as a miss. Prefer
// the named failing CI section, even when its diagnostics are on stdout and
// the wrapper is on stderr. Otherwise retain leading context from both streams
// rather than silently replacing an unknown tool's output with make's trailer.
func fallbackFailureEvidence(stdout, stderr []byte, preferred diagnosticRange) (diagnosticRange, []string) {
	section := failedCISection(stdout, stderr)
	var best diagnosticRange
	bestUseful := false
	var contexts []string
	streams := []struct {
		name string
		data []byte
	}{{"stderr", stderr}, {"stdout", stdout}}
	if preferred.stream == "stdout" {
		streams[0], streams[1] = streams[1], streams[0]
	}
	for _, stream := range streams {
		anchor := 0
		if preferred.priority == specificitySourceFinding && preferred.stream == stream.name {
			anchor = preferred.start
		}
		start, end := contextWindow(stream.data, section, anchor)
		if start == end {
			continue
		}
		raw := string(stream.data[start:end])
		useful := hasDiagnosticContext(raw)
		if best.text == "" || (useful && !bestUseful) {
			bestUseful = useful
			best = diagnosticRange{text: boundDiagnostic(cleanOutputLine(raw)), stream: stream.name, start: start, end: end}
		}
		if useful {
			context := cleanOutputLine(raw)
			if anchor > 0 && len(stream.data) > maxFailureDigestBytes || end < len(stream.data) && end-start >= maxFailureDigestBytes-3 {
				context += "\n... (failure evidence truncated; see output artifacts)"
			}
			contexts = append(contexts, context)
		}
	}
	if len(contexts) == 0 && best.text != "" {
		contexts = append(contexts, best.text)
	}
	return best, boundFailureDigest(contexts)
}

func failedCISection(streams ...[]byte) string {
	var section string
	for _, data := range streams {
		for _, line := range splitOutputLines(data) {
			if match := failedCISectionPattern.FindStringSubmatch(cleanOutputLine(line.text)); len(match) > 1 {
				section = match[1]
			}
		}
	}
	return section
}

func contextWindow(data []byte, section string, anchor int) (int, int) {
	start, end := 0, len(data)
	if section != "" {
		active := false
		for _, line := range splitOutputLines(data) {
			text := cleanOutputLine(line.text)
			if text == "==> "+section {
				start, end, active = line.end, len(data), true
				continue
			}
			if active && (strings.HasPrefix(text, "<== "+section+" (") || strings.HasPrefix(text, "==> ")) {
				end = line.start
				break
			}
		}
	}
	if end-start > maxFailureDigestBytes && anchor > start && anchor < end {
		start = anchor
	}
	if end-start > maxFailureDigestBytes {
		end = start + maxFailureDigestBytes
		for end > start && !utf8.Valid(data[start:end]) {
			end--
		}
	}
	return start, end
}

func hasDiagnosticContext(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		line = cleanOutputLine(line)
		if line != "" && !buildFailurePattern.MatchString(line) && !buildSummaryPattern.MatchString(line) &&
			!strings.HasPrefix(line, "exit status ") && !failedCISectionPattern.MatchString(line) {
			return true
		}
	}
	return false
}

// Keep the truncation marker inside the advertised byte cap, including when a
// single tool emits a diagnostic longer than the entire evidence budget.
func boundFailureDigest(lines []string) []string {
	text := strings.Join(lines, "\n")
	if len(text) <= maxFailureDigestBytes {
		return lines
	}
	const marker = "\n... (failure evidence truncated; see output artifacts)"
	end := maxFailureDigestBytes - len(marker)
	for end > 0 && !utf8.ValidString(text[:end]) {
		end--
	}
	return strings.Split(text[:end]+marker, "\n")
}
