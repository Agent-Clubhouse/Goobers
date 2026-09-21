package findingresponse

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/learning"
)

// Output is the result output carrying the implementer's per-finding account.
const Output = "findingResponses"

// Response accounts for one 1-based finding in the triggering learning episode.
type Response struct {
	Finding     int    `json:"finding"`
	Disposition string `json:"disposition"`
	Detail      string `json:"detail"`
}

// Episode identifies the latest projected learning episode and its findings.
type Episode struct {
	ID       string
	Findings []apiv1.Finding
}

// ArtifactBytes resolves an artifact pointer to its retained bytes.
type ArtifactBytes func(apiv1.ArtifactPointer) ([]byte, error)

// Parse decodes the JSON or line-oriented finding-response form.
func Parse(raw string) ([]Response, error) {
	trimmed := strings.TrimSpace(raw)
	var responses []Response
	jsonErr := json.Unmarshal([]byte(trimmed), &responses)
	if jsonErr == nil {
		return responses, nil
	}
	lineResponses, lineErr := parseLines(trimmed)
	if lineErr == nil && len(lineResponses) > 0 {
		return lineResponses, nil
	}
	return nil, fmt.Errorf("decode JSON array: %w (a line-oriented \"N: disposition: detail\" form is also accepted)", jsonErr)
}

// Validate requires exactly one substantive response for every finding.
func Validate(findings []apiv1.Finding, raw string) ([]Response, error) {
	return validate(findings, raw, false)
}

// ValidateWithAdditional requires every triggering finding to be accounted for
// while preserving the remediation command's compatible ability to record
// findings raised later in the same cycle.
func ValidateWithAdditional(findings []apiv1.Finding, raw string) ([]Response, error) {
	return validate(findings, raw, true)
}

func validate(findings []apiv1.Finding, raw string, allowAdditional bool) ([]Response, error) {
	if strings.TrimSpace(raw) == "" {
		if len(findings) == 0 {
			return []Response{}, nil
		}
		return nil, fmt.Errorf("latest remediation result omitted %s for %d finding(s)", Output, len(findings))
	}
	responses, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	seen := make(map[int]bool, len(responses))
	for i := range responses {
		response := &responses[i]
		response.Disposition = strings.ToLower(strings.TrimSpace(response.Disposition))
		response.Detail = strings.TrimSpace(response.Detail)
		if response.Finding < 1 {
			return nil, fmt.Errorf("response %d names finding %d, want a 1-based finding number", i+1, response.Finding)
		}
		if response.Finding > len(findings) && !allowAdditional {
			return nil, fmt.Errorf("response %d names finding %d, but the triggering episode has %d finding(s)",
				i+1, response.Finding, len(findings))
		}
		if seen[response.Finding] {
			return nil, fmt.Errorf("finding %d is accounted for more than once", response.Finding)
		}
		seen[response.Finding] = true
		if response.Disposition != "addressed" && response.Disposition != "declined" {
			return nil, fmt.Errorf("finding %d disposition is %q, want addressed or declined", response.Finding, response.Disposition)
		}
		if response.Detail == "" {
			return nil, fmt.Errorf("finding %d has no detail describing what changed or why it was declined", response.Finding)
		}
		if response.Disposition == "declined" && !hasDeclineClassification(response.Detail) {
			return nil, fmt.Errorf("finding %d decline must explicitly classify it as obsolete, environmental, flaky, or non-actionable",
				response.Finding)
		}
	}

	for i := range findings {
		if !seen[i+1] {
			return nil, fmt.Errorf("%s has no response; every one of the verdict's %d finding(s) needs exactly one",
				describeFinding(i+1, findings[i]), len(findings))
		}
	}
	sort.Slice(responses, func(i, j int) bool {
		return responses[i].Finding < responses[j].Finding
	})
	return responses, nil
}

func hasDeclineClassification(detail string) bool {
	detail = strings.ToLower(detail)
	for _, classification := range []string{"obsolete", "environmental", "flaky", "non-actionable", "nonactionable"} {
		if strings.Contains(detail, classification) {
			return true
		}
	}
	return false
}

// ValidateResult validates a remediation result against the latest learning
// episode in its projected context. An episode pointer that cannot be read or
// decoded is an error: feedback accounting must never silently disappear.
func ValidateResult(pointers []apiv1.ContextPointer, result apiv1.ResultEnvelope, resolve ArtifactBytes) (Episode, error) {
	var latest learning.Episode
	var sawEpisode bool
	for _, pointer := range pointers {
		class, _ := apiv1.ClassifyContextPointer(pointer.Name)
		if class != apiv1.ContextPointerLearningEpisode {
			continue
		}
		sawEpisode = true
		if pointer.Artifact == nil {
			return Episode{}, fmt.Errorf("learning episode pointer %q has no artifact", pointer.Name)
		}
		if resolve == nil {
			return Episode{}, fmt.Errorf("learning episode pointer %q cannot be resolved", pointer.Name)
		}
		data, err := resolve(*pointer.Artifact)
		if err != nil {
			return Episode{}, fmt.Errorf("read learning episode pointer %q: %w", pointer.Name, err)
		}
		var episode learning.Episode
		if err := json.Unmarshal(data, &episode); err != nil {
			return Episode{}, fmt.Errorf("decode learning episode pointer %q: %w", pointer.Name, err)
		}
		if episode.Schema != learning.EpisodeSchema {
			return Episode{}, fmt.Errorf("learning episode pointer %q has schema %q, want %q",
				pointer.Name, episode.Schema, learning.EpisodeSchema)
		}
		if episode.SourceSeq > latest.SourceSeq {
			latest = episode
		}
	}
	if !sawEpisode {
		return Episode{}, nil
	}
	projected := Episode{ID: latest.ID, Findings: latest.Findings}
	raw, _ := result.Outputs[Output].(string)
	if _, err := Validate(latest.Findings, raw); err != nil {
		return projected, err
	}
	return projected, nil
}

func parseLines(raw string) ([]Response, error) {
	var out []Response
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "-*\t "))
		if line == "" {
			continue
		}
		line = strings.TrimPrefix(line, "#")
		numberPart, rest, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("line %q is not \"<n>: <disposition>: <detail>\"", line)
		}
		number, err := strconv.Atoi(strings.TrimSpace(numberPart))
		if err != nil {
			return nil, fmt.Errorf("line %q does not start with a finding number", line)
		}
		dispositionPart, detail, ok := strings.Cut(rest, ":")
		if !ok {
			return nil, fmt.Errorf("line %q has no detail after the disposition", line)
		}
		out = append(out, Response{
			Finding: number, Disposition: strings.TrimSpace(dispositionPart), Detail: strings.TrimSpace(detail),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no finding response lines found")
	}
	return out, nil
}

func describeFinding(number int, finding apiv1.Finding) string {
	if finding.Message == "" {
		return fmt.Sprintf("verdict finding %d", number)
	}
	if finding.Location == "" {
		return fmt.Sprintf("verdict finding %d (%s)", number, finding.Message)
	}
	return fmt.Sprintf("verdict finding %d (%s at %s)", number, finding.Message, finding.Location)
}
