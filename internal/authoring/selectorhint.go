package authoring

import (
	"fmt"
	"sort"
	"strings"
)

// maxReportedCandidates bounds how many valid names an unknown-selector error
// spells out, so a wide schema node stays readable in a terminal.
const maxReportedCandidates = 12

// SelectorError reports an unresolvable selector together with the segment
// that failed, the names that are valid at that segment, and the closest
// near-miss selector. It wraps ErrUnknownSelector, so callers keep matching it
// with errors.Is.
type SelectorError struct {
	// Selector is the selector as the author wrote it.
	Selector string
	// Segment is the failing segment name.
	Segment string
	// Prefix is the dotted selector that resolved before the failure.
	Prefix string
	// Candidates are the valid names at the failing segment.
	Candidates []string
	// CandidateLabel describes what the candidates are, such as "valid kinds".
	CandidateLabel string
	// Suggestion is the nearest valid selector, when one is close enough.
	Suggestion string

	detail string
}

func (e *SelectorError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %q", ErrUnknownSelector, e.Selector)
	if e.detail != "" {
		fmt.Fprintf(&b, ": %s", e.detail)
	}
	if e.Suggestion != "" {
		fmt.Fprintf(&b, "; did you mean %q?", e.Suggestion)
	}
	if len(e.Candidates) > 0 {
		label := e.CandidateLabel
		if label == "" {
			label = "valid names"
		}
		shown := e.Candidates
		omitted := 0
		if len(shown) > maxReportedCandidates {
			omitted = len(shown) - maxReportedCandidates
			shown = shown[:maxReportedCandidates]
		}
		fmt.Fprintf(&b, "; %s: %s", label, strings.Join(shown, ", "))
		if omitted > 0 {
			fmt.Fprintf(&b, " (+%d more)", omitted)
		}
	}
	return b.String()
}

func (e *SelectorError) Unwrap() error { return ErrUnknownSelector }

// unknownKindSelector reports a selector whose first segment names no
// embedded schema kind.
func unknownKindSelector(selector string, parts []selectorPart, kinds []string) error {
	return &SelectorError{
		Selector:       selector,
		Segment:        parts[0].name,
		Candidates:     kinds,
		CandidateLabel: "valid kinds",
		Suggestion:     suggestSelector(selector, parts, 0, nearestName(parts[0].name, kinds)),
		detail:         fmt.Sprintf("no schema kind %q", parts[0].name),
	}
}

// unknownFieldError reports a selector segment that names no field on the
// schema node its prefix resolved to. When that node is an array, the fields
// live under its elements, so the reported prefix, candidates, and suggestion
// all carry the `[]` the author omitted.
func (r *registry) unknownFieldError(selector string, parts []selectorPart, index int, doc *schemaDocument, node map[string]any) error {
	prefixParts := make([]selectorPart, index)
	copy(prefixParts, parts[:index])
	if itemDoc, _, itemResolved, found, err := r.resolveItems(doc, node, 0); found && err == nil {
		prefixParts[index-1].element = true
		doc, node = itemDoc, itemResolved
	}
	candidates := r.propertyNames(doc, node, 0)
	prefix := renderSelector(selector, prefixParts)

	suggestion := ""
	if nearest := nearestName(parts[index].name, candidates); nearest != "" {
		suggested := append(append([]selectorPart{}, prefixParts...), parts[index:]...)
		suggested[index].name = nearest
		suggestion = renderSelector(selector, suggested)
	}
	return &SelectorError{
		Selector:       selector,
		Segment:        parts[index].name,
		Prefix:         prefix,
		Candidates:     candidates,
		CandidateLabel: fmt.Sprintf("valid fields at %q", prefix),
		Suggestion:     suggestion,
		detail:         fmt.Sprintf("no field %q under %q", parts[index].name, prefix),
	}
}

// notAnArraySelector reports an element selector (`[]`) applied to a field
// that is not an array.
func notAnArraySelector(selector string, parts []selectorPart, index int) error {
	scalar := make([]selectorPart, len(parts))
	copy(scalar, parts)
	scalar[index].element = false
	return &SelectorError{
		Selector:   selector,
		Segment:    parts[index].name,
		Prefix:     renderSelector(selector, parts[:index]),
		Suggestion: renderSelector(selector, scalar),
		detail:     fmt.Sprintf("field %q is not an array, so it has no [] elements", renderSelector(selector, scalar[:index+1])),
	}
}

// suggestSelector rebuilds the selector with the failing segment replaced by
// name, preserving the separator style and element markers the author used.
func suggestSelector(selector string, parts []selectorPart, index int, name string) string {
	if name == "" {
		return ""
	}
	suggested := make([]selectorPart, len(parts))
	copy(suggested, parts)
	suggested[index].name = name
	return renderSelector(selector, suggested)
}

func renderSelector(selector string, parts []selectorPart) string {
	rendered := selectorString(parts, true)
	if strings.Contains(selector, "/") {
		rendered = strings.ReplaceAll(rendered, ".", "/")
	}
	return rendered
}

// nearestName returns the candidate closest to name, or "" when none is close
// enough to be worth suggesting.
func nearestName(name string, candidates []string) string {
	folded := strings.ToLower(name)
	best, bestDistance := "", 0
	for _, candidate := range candidates {
		distance := editDistance(folded, strings.ToLower(candidate))
		if distance > nearMissThreshold(folded) {
			continue
		}
		if best == "" || distance < bestDistance ||
			(distance == bestDistance && candidate < best) {
			best, bestDistance = candidate, distance
		}
	}
	return best
}

// nearMissThreshold scales the accepted edit distance with the length of the
// typed name, so a short name does not match an unrelated short candidate.
func nearMissThreshold(name string) int {
	switch {
	case len(name) < 3:
		return 1
	case len(name) < 6:
		return 2
	default:
		return 3
	}
}

func editDistance(a, b string) int {
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current[0] = i
		for j := 1; j <= len(b); j++ {
			substitution := previous[j-1]
			if a[i-1] != b[j-1] {
				substitution++
			}
			current[j] = min(min(previous[j]+1, current[j-1]+1), substitution)
		}
		previous, current = current, previous
	}
	return previous[len(b)]
}

// propertyNames lists every field name reachable at node, including through
// allOf/oneOf/anyOf alternatives, sorted for a stable diagnostic.
func (r *registry) propertyNames(doc *schemaDocument, node map[string]any, depth int) []string {
	names := make(map[string]bool)
	r.collectPropertyNames(doc, node, names, depth)
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	return sorted
}

func (r *registry) collectPropertyNames(doc *schemaDocument, node map[string]any, names map[string]bool, depth int) {
	if depth > 32 {
		return
	}
	doc, node, err := r.resolve(doc, node)
	if err != nil {
		return
	}
	if properties, ok := node["properties"].(map[string]any); ok {
		for name := range properties {
			names[name] = true
		}
	}
	for _, keyword := range []string{"allOf", "oneOf", "anyOf"} {
		alternatives, _ := node[keyword].([]any)
		for _, value := range alternatives {
			alternative, ok := value.(map[string]any)
			if !ok {
				continue
			}
			r.collectPropertyNames(doc, alternative, names, depth+1)
		}
	}
}
