package workflow

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// ErrChildSelectionExecutionUnsupported identifies declarations whose selection
// and immutable-base runtime is not yet available.
var ErrChildSelectionExecutionUnsupported = errors.New("child-workflow agent profiles and named bases are not executable yet")

// RefuseChildSelectionExecution protects start and recovery before side effects.
// Compiling a policy for authoring does not imply executable selection support.
func RefuseChildSelectionExecution(spec apiv1.WorkflowSpec) error {
	for _, task := range spec.Tasks {
		p := task.ChildWorkflows
		if p != nil && (len(p.AllowedAgentProfiles) != 0 || len(p.AllowedBases) != 0) {
			return fmt.Errorf("task %q: %w", task.Name, ErrChildSelectionExecutionUnsupported)
		}
	}
	return nil
}

var childSelectionName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

func childSelectionProblems(policy *apiv1.ChildWorkflowPolicy, allowedGoobers map[string]bool) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	if len(policy.AllowedAgentProfiles) > 32 {
		add("allowedAgentProfiles must contain at most 32 entries")
	}
	names := map[string]bool{}
	tuples := map[[3]string]bool{}
	for _, p := range policy.AllowedAgentProfiles {
		if !childSelectionName.MatchString(p.Name) || names[p.Name] {
			add("allowedAgentProfiles requires valid unique names")
		}
		names[p.Name] = true
		if !allowedGoobers[p.Goober] {
			add("allowedAgentProfiles goober must occur in allowedGoobers")
		}
		if !childSelectionName.MatchString(string(p.Harness)) {
			add("allowedAgentProfiles requires a harness identifier")
		}
		if p.Model == "" || len(p.Model) > 256 || !utf8.ValidString(p.Model) ||
			strings.IndexFunc(p.Model, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
			add("allowedAgentProfiles requires a bounded model identifier without whitespace or controls")
		}
		tuple := [3]string{p.Goober, string(p.Harness), p.Model}
		if tuples[tuple] {
			add("allowedAgentProfiles contains a duplicate goober/harness/model tuple")
		}
		tuples[tuple] = true
	}
	if len(policy.AllowedBases) > 32 {
		add("allowedBases must contain at most 32 entries")
	}
	names = map[string]bool{}
	for _, base := range policy.AllowedBases {
		if !childSelectionName.MatchString(base.Name) || names[base.Name] {
			add("allowedBases requires valid unique names")
		}
		names[base.Name] = true
		if !validChildBaseRef(base.Ref) {
			add("allowedBases requires a fully qualified branch or tag ref without revision expressions")
		}
	}
	return problems
}

// This pure check is deliberately limited to full branch and tag refs. It does
// not resolve, fetch, or interpret a revision against ambient repository state.
func validChildBaseRef(ref string) bool {
	if len(ref) > 1024 || !utf8.ValidString(ref) ||
		(!strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/")) ||
		strings.Contains(ref, "..") || strings.Contains(ref, "@{") ||
		strings.ContainsAny(ref, ` ~^:?*[\`) ||
		strings.IndexFunc(ref, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return false
	}
	for _, part := range strings.Split(ref, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}
