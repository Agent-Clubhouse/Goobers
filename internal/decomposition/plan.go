package decomposition

import (
	"fmt"
	"sort"
	"strings"
)

// PlanSchemaV1 is the only decomposition-plan schema version this build reads
// or writes (design doc §4).
const PlanSchemaV1 = "v1"

var supportedPlanSchemaVersions = map[string]bool{
	PlanSchemaV1: true,
}

// SupportedPlanSchemaVersion reports whether version is a plan schema this
// build can validate. Fails closed on anything unlisted, including empty —
// an unrecognized or absent version means the plan's shape cannot be trusted
// to match what the rest of this struct assumes.
func SupportedPlanSchemaVersion(version string) bool {
	return supportedPlanSchemaVersions[version]
}

// PlanSelection is the subset of the select-source Selection artifact (DEC-1)
// a plan must bind itself to, so validate-plan can confirm design-slices
// designed against the exact source and parent snapshot select-source claimed
// — not a stale or substituted one.
type PlanSelection struct {
	Mode                SelectionMode `json:"mode"`
	SourceRunID         string        `json:"sourceRunId,omitempty"`
	IssueSnapshotDigest string        `json:"issueSnapshotDigest"`
}

// ChildPlan is one proposed child issue.
type ChildPlan struct {
	Key    string   `json:"key"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels,omitempty"`
	// AcceptanceCriteria and ValidationBoundary are required, non-empty prose:
	// the design doc requires every child to "describe one coherent change
	// that can produce one PR and name a plausible validation boundary" —
	// this validator can only check that a boundary was actually stated. It
	// is not a substitute for the coherence judgment.
	AcceptanceCriteria string `json:"acceptanceCriteria"`
	ValidationBoundary string `json:"validationBoundary"`
	// DependsOn entries reference either a sibling child's Key or an
	// already-existing issue ID (design doc §4: "an acyclic dependency graph
	// using only child keys or existing issue IDs").
	DependsOn []string `json:"dependsOn,omitempty"`
}

// Plan is the versioned decomposition-plan artifact design-slices emits and
// validate-plan checks (design doc §4).
type Plan struct {
	SchemaVersion string        `json:"schemaVersion"`
	Selection     PlanSelection `json:"selection"`
	Parent        ParentRef     `json:"parent"`
	Summary       string        `json:"summary"`
	Children      []ChildPlan   `json:"children"`
	// UnresolvedDecision names a product decision that must be made before a
	// decomposition can be published.
	UnresolvedDecision string `json:"unresolvedDecision,omitempty"`
	// SingleReplacementReason is set only when the plan deliberately rewrites
	// the parent into one smaller replacement instead of splitting it — the
	// design doc's explicit exception to the at-least-two-children rule.
	// Empty means the ordinary multi-child rule applies.
	SingleReplacementReason string `json:"singleReplacementReason,omitempty"`
}

// LiveParentSnapshot is the parent issue's current content, fetched fresh at
// validation time — never the plan's own claims about it — used to detect a
// conflicting edit since selection (design doc §4's last paragraph).
type LiveParentSnapshot struct {
	ID     string
	Title  string
	Body   string
	Labels []string
	State  string
}

// ParentConflict reports that the live parent no longer matches what
// select-source observed. Validation must park with this exact conflict, not
// silently regenerate the plan against the new content.
type ParentConflict struct {
	Reason string
}

// ValidationResult is validate-plan's outcome. A conflict is distinct from an
// ordinary structural Errors entry: it means the plan may be well-formed but
// was designed against parent content that has since changed, and must route
// to park-for-human rather than back through design-slices' repass loop.
type ValidationResult struct {
	Valid              bool
	Errors             []string
	Conflict           *ParentConflict
	UnresolvedDecision bool
	SchemaInvalid      bool
}

// Repassable reports whether design-slices can safely correct this result.
func (r ValidationResult) Repassable() bool {
	return !r.Valid && !r.SchemaInvalid && r.Conflict == nil && !r.UnresolvedDecision
}

// disallowedChildLabels are publisher-owned trust/readiness/status markers —
// design doc §4: "trust and readiness labels are publisher-owned and cannot
// be requested by the model."
var disallowedChildLabelPrefixes = []string{"goobers:", "goobers/status:"}

const minChildBodyLength = 20

// ValidatePlan checks plan against every design doc §4 rule and binds it to
// selection (DEC-1's Selection artifact) and the freshly fetched live parent.
// It accumulates every structural finding rather than failing on the first,
// since design-slices' repass loop benefits from a complete list (design doc
// §3.2). An unsupported/malformed schemaVersion is the one fail-fast case: the
// rest of this type's shape cannot be trusted for a version this build does
// not recognize.
func ValidatePlan(plan Plan, selection Selection, live LiveParentSnapshot) ValidationResult {
	if !SupportedPlanSchemaVersion(plan.SchemaVersion) {
		return ValidationResult{
			Errors:        []string{fmt.Sprintf("unsupported or malformed plan schemaVersion %q", plan.SchemaVersion)},
			SchemaInvalid: true,
		}
	}

	var errs []string
	errs = append(errs, validatePlanBinding(plan, selection)...)
	errs = append(errs, validateChildren(plan)...)

	digest, err := IssueSnapshotDigest(live.ID, live.Title, live.Body, live.Labels, live.State)
	var conflict *ParentConflict
	if err != nil {
		errs = append(errs, fmt.Sprintf("compute live parent digest: %v", err))
	} else if digest != selection.IssueSnapshotDigest {
		conflict = &ParentConflict{Reason: fmt.Sprintf(
			"parent %s changed since selection (observed digest %s, live digest %s)",
			live.ID, selection.IssueSnapshotDigest, digest,
		)}
	}

	return ValidationResult{
		Valid:              len(errs) == 0 && conflict == nil && strings.TrimSpace(plan.UnresolvedDecision) == "",
		Errors:             errs,
		Conflict:           conflict,
		UnresolvedDecision: strings.TrimSpace(plan.UnresolvedDecision) != "",
	}
}

// validatePlanBinding checks design doc §4's selection-binding rule: mode,
// source identity, parent identity, parent observed revision, and issue
// snapshot digest must match the selector artifact.
func validatePlanBinding(plan Plan, selection Selection) []string {
	var errs []string
	if plan.Selection.Mode != selection.Mode {
		errs = append(errs, fmt.Sprintf("plan selection mode %q does not match selector artifact mode %q", plan.Selection.Mode, selection.Mode))
	}
	if plan.Selection.SourceRunID != selection.SourceRunID {
		errs = append(errs, fmt.Sprintf("plan selection sourceRunId %q does not match selector artifact %q", plan.Selection.SourceRunID, selection.SourceRunID))
	}
	if plan.Selection.IssueSnapshotDigest != selection.IssueSnapshotDigest {
		errs = append(errs, "plan selection issueSnapshotDigest does not match selector artifact")
	}
	if plan.Parent != selection.Parent {
		errs = append(errs, fmt.Sprintf("plan parent %+v does not match selector artifact parent %+v", plan.Parent, selection.Parent))
	}
	return errs
}

func validateChildren(plan Plan) []string {
	var errs []string

	if len(plan.Children) == 0 {
		return append(errs, "plan has no children")
	}
	if plan.SingleReplacementReason == "" {
		if len(plan.Children) < 2 {
			errs = append(errs, "plan must propose at least two children unless singleReplacementReason explains a single smaller replacement")
		}
	} else if len(plan.Children) != 1 {
		errs = append(errs, "a plan with singleReplacementReason set must propose exactly one replacement child")
	}

	keys := make(map[string]bool, len(plan.Children))
	seenContent := make(map[string]string, len(plan.Children))
	for _, child := range plan.Children {
		if child.Key == "" {
			errs = append(errs, "child has an empty key")
		} else if keys[child.Key] {
			errs = append(errs, fmt.Sprintf("duplicate child key %q", child.Key))
		}
		keys[child.Key] = true

		if strings.TrimSpace(child.Title) == "" {
			errs = append(errs, fmt.Sprintf("child %q has an empty title", child.Key))
		}
		if strings.TrimSpace(child.AcceptanceCriteria) == "" {
			errs = append(errs, fmt.Sprintf("child %q has empty acceptance criteria", child.Key))
		}
		if strings.TrimSpace(child.ValidationBoundary) == "" {
			errs = append(errs, fmt.Sprintf("child %q names no validation boundary", child.Key))
		}
		if len(strings.TrimSpace(child.Body)) < minChildBodyLength {
			errs = append(errs, fmt.Sprintf("child %q body is too short to describe a coherent single-PR change", child.Key))
		}
		content := strings.TrimSpace(child.Title) + "\x00" + strings.TrimSpace(child.Body)
		if child.Key != "" {
			if dupKey, seen := seenContent[content]; seen {
				errs = append(errs, fmt.Sprintf("child %q duplicates child %q (identical title and body)", child.Key, dupKey))
			} else {
				seenContent[content] = child.Key
			}
		}

		for _, label := range child.Labels {
			if !allowedChildLabel(label) {
				errs = append(errs, fmt.Sprintf("child %q requests a publisher-owned or non-allowlisted label %q", child.Key, label))
			}
		}
	}

	errs = append(errs, findDependencyCycles(plan.Children)...)
	return errs
}

// allowedChildLabel enforces "only allowlisted area/type labels" (design doc
// §4): area:*/type:* labels describe scope and kind, which the model may
// propose; every trust/readiness/status marker is publisher-owned.
func allowedChildLabel(label string) bool {
	for _, prefix := range disallowedChildLabelPrefixes {
		if strings.HasPrefix(label, prefix) {
			return false
		}
	}
	return strings.HasPrefix(label, "area:") || strings.HasPrefix(label, "type:")
}

// findDependencyCycles checks the dependency graph over child keys only —
// DependsOn entries that reference an existing issue ID rather than a
// sibling key are external references and cannot themselves complete a cycle
// within this plan.
func findDependencyCycles(children []ChildPlan) []string {
	edges := make(map[string][]string, len(children))
	for _, child := range children {
		edges[child.Key] = append(edges[child.Key], child.DependsOn...)
	}
	keys := make([]string, 0, len(children))
	for _, child := range children {
		keys = append(keys, child.Key)
	}
	sort.Strings(keys) // deterministic error ordering

	const (
		unvisited = 0
		visiting  = 1
		done      = 2
	)
	state := make(map[string]int, len(keys))
	var errs []string
	var path []string

	var visit func(key string) bool
	visit = func(key string) bool {
		switch state[key] {
		case done:
			return false
		case visiting:
			return true
		}
		state[key] = visiting
		path = append(path, key)
		for _, dep := range edges[key] {
			if _, isChild := findChildIndex(children, dep); !isChild {
				continue // external issue reference, not a plan-internal edge
			}
			if visit(dep) {
				return true
			}
		}
		path = path[:len(path)-1]
		state[key] = done
		return false
	}

	for _, key := range keys {
		if state[key] != unvisited {
			continue
		}
		path = nil
		if visit(key) {
			errs = append(errs, fmt.Sprintf("dependency cycle detected involving %s", strings.Join(path, " -> ")))
			return errs // one report is enough; the plan is rejected either way
		}
	}
	return errs
}

func findChildIndex(children []ChildPlan, key string) (int, bool) {
	for i, child := range children {
		if child.Key == key {
			return i, true
		}
	}
	return -1, false
}
