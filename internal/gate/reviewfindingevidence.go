package gate

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

type journalDirectory interface {
	Dir() string
}

func journalDir(j Journal) string {
	if provider, ok := j.(journalDirectory); ok {
		return provider.Dir()
	}
	return ""
}

func disproveReviewerFindings(verdict apiv1.Verdict, pointers []apiv1.ContextPointer, resolve ArtifactBytes, gateName string) (apiv1.Verdict, bool) {
	if len(verdict.Findings) == 0 || resolve == nil {
		return verdict, false
	}
	source := reviewerPatchSource(pointers, resolve, gateName)
	if len(source) == 0 {
		return verdict, false
	}

	remaining := make([]apiv1.Finding, 0, len(verdict.Findings))
	var evidence []string
	for _, finding := range verdict.Findings {
		line, ok := source.lineAt(finding.Location)
		if !isInvalidJSONFinding(finding) || !ok {
			remaining = append(remaining, finding)
			continue
		}
		raw, ok := singleRawString(line)
		if !ok || !json.Valid([]byte(raw)) {
			remaining = append(remaining, finding)
			continue
		}
		evidence = append(evidence, fmt.Sprintf(
			"%s: exact raw-string bytes parse successfully with encoding/json.Valid",
			finding.Location,
		))
	}
	if len(evidence) == 0 {
		return verdict, false
	}

	verdict.Findings = remaining
	note := ReasonFindingDisproven + ": " + strings.Join(evidence, "; ")
	if verdict.Rationale == "" {
		verdict.Rationale = note
	} else {
		verdict.Rationale += "\n\n" + note
	}
	allDisproven := len(remaining) == 0
	if allDisproven {
		verdict.Decision = apiv1.VerdictPass
	}
	return verdict, allDisproven
}

func isInvalidJSONFinding(finding apiv1.Finding) bool {
	message := strings.ToLower(finding.Message)
	if !strings.Contains(message, "json") {
		return false
	}
	for _, term := range []string{"unparseable", "fails to parse", "cannot parse", "does not parse", "parse error", "syntax error"} {
		if strings.Contains(message, term) {
			return true
		}
	}
	claimsInvalid := strings.Contains(message, "invalid") || strings.Contains(message, "malformed")
	if !claimsInvalid {
		return false
	}
	for _, term := range []string{"escape", "backslash", "quote", "raw string", "raw-string", "literal"} {
		if strings.Contains(message, term) {
			return true
		}
	}
	return false
}

func singleRawString(line string) (string, bool) {
	var values []string
	for start := 0; start < len(line); {
		open := strings.IndexByte(line[start:], '`')
		if open < 0 {
			break
		}
		open += start
		close := strings.IndexByte(line[open+1:], '`')
		if close < 0 {
			return "", false
		}
		close += open + 1
		values = append(values, line[open+1:close])
		start = close + 1
	}
	if len(values) != 1 {
		return "", false
	}
	return values[0], true
}

type patchSource map[string]map[int]string

func reviewerPatchSource(pointers []apiv1.ContextPointer, resolve ArtifactBytes, gateName string) patchSource {
	combined := make(patchSource)
	for _, pointer := range pointers {
		if pointer.Name != gateName+".diff" || pointer.Artifact == nil || !isDiffPointer(pointer) {
			continue
		}
		data, err := resolve(*pointer.Artifact)
		if err != nil {
			continue
		}
		for path, lines := range parsePatchSource(string(data)) {
			if combined[path] == nil {
				combined[path] = make(map[int]string)
			}
			for line, text := range lines {
				combined[path][line] = text
			}
		}
	}
	return combined
}

func isDiffPointer(pointer apiv1.ContextPointer) bool {
	mediaType := strings.ToLower(pointer.Artifact.MediaType)
	return mediaType == "text/x-diff" ||
		mediaType == "text/x-patch" ||
		strings.HasSuffix(strings.ToLower(pointer.Name), ".diff") ||
		strings.HasSuffix(strings.ToLower(pointer.Name), ".patch")
}

func parsePatchSource(patch string) patchSource {
	source := make(patchSource)
	path := ""
	nextLine := 0
	for _, raw := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(raw, "+++ b/"):
			path = strings.TrimPrefix(raw, "+++ b/")
			nextLine = 0
		case strings.HasPrefix(raw, "@@ "):
			nextLine = patchHunkStart(raw)
		case path == "" || nextLine == 0:
			continue
		case strings.HasPrefix(raw, "+") && !strings.HasPrefix(raw, "+++"):
			if source[path] == nil {
				source[path] = make(map[int]string)
			}
			source[path][nextLine] = raw[1:]
			nextLine++
		case strings.HasPrefix(raw, " "):
			if source[path] == nil {
				source[path] = make(map[int]string)
			}
			source[path][nextLine] = raw[1:]
			nextLine++
		}
	}
	return source
}

func patchHunkStart(header string) int {
	plus := strings.Index(header, " +")
	if plus < 0 {
		return 0
	}
	start := plus + 2
	end := start
	for end < len(header) && header[end] >= '0' && header[end] <= '9' {
		end++
	}
	if end == start {
		return 0
	}
	line, err := strconv.Atoi(header[start:end])
	if err != nil {
		return 0
	}
	return line
}

func (source patchSource) lineAt(location string) (string, bool) {
	colon := strings.LastIndexByte(location, ':')
	if colon <= 0 || colon == len(location)-1 {
		return "", false
	}
	line, err := strconv.Atoi(location[colon+1:])
	if err != nil || line < 1 {
		return "", false
	}
	lines, ok := source[location[:colon]]
	if !ok {
		return "", false
	}
	text, ok := lines[line]
	return text, ok
}
