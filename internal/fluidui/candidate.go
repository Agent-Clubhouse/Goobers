package fluidui

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

// Outcomes of one customization attempt (#8418). The workflow's agent stage
// writes either a customization document or an explicit unsupported marker;
// anything else is invalid.
const (
	OutcomeCandidate   = "candidate"
	OutcomeUnsupported = "unsupported"
	OutcomeInvalid     = "invalid"
)

// Unmet-need labels are short slugs so they can be counted across users
// without carrying any of the request text that produced them.
const (
	MaxUnmetNeedLen    = 48
	maxUnmetNeedTokens = 4
	UnmetNeedUnlabeled = "unlabeled"
	maxErrorSummaryLen = 512
)

var unmetNeedSeparator = regexp.MustCompile(`[^a-z0-9]+`)

// CandidateResult is the validate-candidate stage's result file and the
// daemon's own re-validation of it. Candidate is nested so the stage's flat
// outputs (which the gate reads) never carry the document.
type CandidateResult struct {
	Valid        bool           `json:"valid"`
	Outcome      string         `json:"outcome"`
	Candidate    *Customization `json:"candidate,omitempty"`
	UnmetNeed    string         `json:"unmetNeed,omitempty"`
	Errors       []string       `json:"errors,omitempty"`
	ErrorSummary string         `json:"errorSummary,omitempty"`
	// Repassable is true only for invalid output: the workflow gives the agent
	// one more attempt with these errors, then fails the run.
	Repassable bool `json:"repassable"`
}

type unsupportedMarker struct {
	Unsupported bool   `json:"unsupported"`
	UnmetNeed   string `json:"unmetNeed"`
}

// EvaluateCandidate classifies an agent's output as a valid customization, an
// explicit unsupported request, or invalid output with its validation errors.
func EvaluateCandidate(data []byte) CandidateResult {
	data = bytes.TrimSpace(data)
	if len(data) > MaxDocumentBytes {
		return invalidCandidate(invalid("$", "output exceeds %d bytes", MaxDocumentBytes))
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal(data, &probe) == nil {
		if _, ok := probe["unsupported"]; ok {
			return evaluateUnsupported(data)
		}
	}
	doc, err := Decode(data)
	if err != nil {
		return invalidCandidate(err)
	}
	return CandidateResult{Valid: true, Outcome: OutcomeCandidate, Candidate: &doc}
}

func evaluateUnsupported(data []byte) CandidateResult {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var marker unsupportedMarker
	if err := decoder.Decode(&marker); err != nil {
		return invalidCandidate(invalid("$", "malformed unsupported marker: %v", err))
	}
	if !marker.Unsupported {
		return invalidCandidate(invalid("unsupported", "must be true when present"))
	}
	return CandidateResult{Valid: true, Outcome: OutcomeUnsupported, UnmetNeed: NormalizeUnmetNeed(marker.UnmetNeed)}
}

func invalidCandidate(err error) CandidateResult {
	summary := err.Error()
	if len(summary) > maxErrorSummaryLen {
		summary = summary[:maxErrorSummaryLen]
	}
	return CandidateResult{Outcome: OutcomeInvalid, Errors: []string{summary}, ErrorSummary: summary, Repassable: true}
}

// NormalizeUnmetNeed reduces a free-form label to a short lowercase slug of at
// most four words, so an agent cannot smuggle request text into aggregates.
func NormalizeUnmetNeed(label string) string {
	tokens := strings.FieldsFunc(unmetNeedSeparator.ReplaceAllString(strings.ToLower(label), " "), func(r rune) bool { return r == ' ' })
	if len(tokens) > maxUnmetNeedTokens {
		tokens = tokens[:maxUnmetNeedTokens]
	}
	slug := strings.Join(tokens, "-")
	if len(slug) > MaxUnmetNeedLen {
		slug = strings.TrimRight(slug[:MaxUnmetNeedLen], "-")
	}
	if slug == "" {
		return UnmetNeedUnlabeled
	}
	return slug
}

// labelEchoesPrompt reports whether a label is just the opening words of the
// request text. Such a label is replaced, so a multi-word prompt never
// escapes through it. Short labels are kept: "dark-mode" is a useful category
// even when the request was literally "dark mode".
func labelEchoesPrompt(label, prompt string) bool {
	return strings.Count(label, "-") >= 2 && label == NormalizeUnmetNeed(prompt)
}

// Surface describes what a customization can change, for the agent that drafts
// one. Panel IDs mirror the Portal's documented panel registry.
func Surface() map[string]any {
	return map[string]any{
		"version":   Version,
		"densities": []string{DensityComfortable, DensityCompact},
		"pages": map[string][]string{
			"overview": {"summary", "attention", "activity", "warnings"},
		},
		"runList": map[string]any{
			"sorts":  []string{SortNewest, SortOldest, SortFailingFirst},
			"phases": []string{"running", "completed", "failed", "aborted", "escalated"},
		},
		"savedViews": map[string]any{
			"max":         MaxSavedViews,
			"nameMaxLen":  MaxViewNameLen,
			"routePrefix": "#/",
			"examples":    []string{"#/runs", "#/workflows", "#/labs"},
		},
		"rules": []string{
			"Output JSON only: a portal.customization/v1 document, or {\"unsupported\": true, \"unmetNeed\": \"short-slug\"}.",
			"Never emit code, scripts, CSS, HTML, URLs outside #/ routes, or authorization changes.",
			"Unknown fields are rejected.",
		},
	}
}
