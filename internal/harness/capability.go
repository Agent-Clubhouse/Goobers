package harness

import (
	"fmt"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// ErrorCodeCapabilityUnsatisfied is the failure code a stage carries when the
// tool surface actually granted to its goober cannot exercise a capability the
// goober declares (#2197). It is deliberately a failure, never a block: a
// capability the harness never provisioned is a system defect an operator
// fixes, and has nothing to do with the content of whatever item the run
// happens to be carrying — routing it through ResultBlocked needs-human-parks
// the run's whole claimed batch for a reason unrelated to any of those items.
const ErrorCodeCapabilityUnsatisfied = "HARNESS_CAPABILITY_UNSATISFIED"

// ToolSurfaceReporter is implemented by adapters that can report the concrete
// model-facing tool IDs a declared allowlist expands to. The capability
// preflight asks the adapter itself rather than re-deriving the expansion, so
// the surface it checks is the one the session will actually get. Adapters
// that cannot report a surface are simply not preflighted.
type ToolSurfaceReporter interface {
	AvailableTools(declared []string) []string
}

// capabilityToolRequirement states what a declared tool group must actually
// expose for a declared capability to be exercisable through it. It is scoped
// to the group on purpose: a goober that declares the capability but not the
// group may legitimately exercise it another way (e.g. the `gh` CLI under the
// shell group), and failing that closed would break working configurations.
// The condition this catches is the one #2184 hit — the group IS declared, and
// silently expands to a surface that cannot do the declared thing.
type capabilityToolRequirement struct {
	capability string
	group      string
	// anyOf are tool-ID suffixes, at least one of which must appear in the
	// expanded surface. Suffixes because adapters namespace concrete IDs by
	// server (e.g. "github-mcp-server-issue_write").
	anyOf []string
}

var capabilityToolRequirements = []capabilityToolRequirement{
	{capability: "github:issues:write", group: "github", anyOf: []string{"issue_write", "sub_issue_write", "add_issue_comment"}},
	{capability: "github:issues:approve", group: "github", anyOf: []string{"issue_write", "add_issue_comment"}},
	{capability: "github:milestones:write", group: "github", anyOf: []string{"issue_write"}},
	{capability: "github:issues:read", group: "github", anyOf: []string{"issue_read", "list_issues", "search_issues"}},
}

// CapabilityUnsatisfiedError names one declared capability the granted tool
// surface cannot exercise.
type CapabilityUnsatisfiedError struct {
	Capability string
	Group      string
	AnyOf      []string
	Surface    []string
}

func (e *CapabilityUnsatisfiedError) Error() string {
	return fmt.Sprintf(
		"harness: declared capability %q is unsatisfiable: the %q tool group is declared but its granted surface exposes none of %s (granted: %s)",
		e.Capability, e.Group, strings.Join(e.AnyOf, ", "), surfaceSummary(e.Surface))
}

func surfaceSummary(surface []string) string {
	if len(surface) == 0 {
		return "(none)"
	}
	names := append([]string(nil), surface...)
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// preflightCapabilityTools verifies that the surface an adapter will expose
// satisfies every declared capability that has a stated tool requirement. It
// runs before the harness session starts, so a provisioning regression fails
// the stage deterministically instead of depending on the model noticing
// mid-session and self-reporting it correctly.
func preflightCapabilityTools(capabilities, declaredTools, surface []string) error {
	if len(capabilities) == 0 || len(declaredTools) == 0 {
		return nil
	}
	declared := make(map[string]struct{}, len(declaredTools))
	for _, tool := range declaredTools {
		declared[strings.ToLower(strings.TrimSpace(tool))] = struct{}{}
	}
	granted := make(map[string]struct{}, len(capabilities))
	for _, capability := range capabilities {
		granted[strings.ToLower(strings.TrimSpace(capability))] = struct{}{}
	}
	for _, req := range capabilityToolRequirements {
		if _, ok := granted[req.capability]; !ok {
			continue
		}
		if _, ok := declared[req.group]; !ok {
			continue
		}
		if surfaceSatisfies(surface, req.anyOf) {
			continue
		}
		return &CapabilityUnsatisfiedError{
			Capability: req.capability,
			Group:      req.group,
			AnyOf:      req.anyOf,
			Surface:    surface,
		}
	}
	return nil
}

func surfaceSatisfies(surface, anyOf []string) bool {
	for _, tool := range surface {
		lower := strings.ToLower(tool)
		for _, want := range anyOf {
			if lower == want || strings.HasSuffix(lower, "-"+want) || strings.HasSuffix(lower, "_"+want) {
				return true
			}
		}
	}
	return false
}

// capabilityPreflight checks this Executor's declared tool allowlist against
// the adapter's own reported surface for the invocation's declared
// capabilities. Adapter-agnostic by construction: any adapter implementing
// ToolSurfaceReporter is preflighted, for any agentic stage.
func (e *Executor) capabilityPreflight(env apiv1.InvocationEnvelope) error {
	reporter, ok := e.adapter.(ToolSurfaceReporter)
	if !ok {
		return nil
	}
	return preflightCapabilityTools(env.Capabilities, e.tools, reporter.AvailableTools(e.tools))
}

// capabilityFailureResult renders a preflight failure as the stage result the
// runner routes to PhaseFailed: non-retryable (the identical invocation
// reproduces the identical surface) and naming the missing capability.
func capabilityFailureResult(err error) apiv1.ResultEnvelope {
	return apiv1.ResultEnvelope{
		Status:  apiv1.ResultFailure,
		Summary: "declared capability is not satisfied by the granted tool surface",
		Error: &apiv1.ErrorInfo{
			Code:      ErrorCodeCapabilityUnsatisfied,
			Retryable: false,
			Message:   err.Error(),
		},
	}
}

// missingCapabilityCodeMarkers are the self-reported error codes that name a
// lost or never-granted tool capability. Matched on the code alone — never on
// prose — so an ordinary dependency block whose message happens to mention a
// tool is untouched.
var missingCapabilityCodeMarkers = []string{
	"MISSING_CAPABILITY",
	"CAPABILITY_MISSING",
	"CAPABILITY_UNAVAILABLE",
	"CAPABILITY_NOT_GRANTED",
	"CAPABILITY_UNSATISFIED",
	"MISSING_TOOL",
	"TOOL_MISSING",
	"TOOL_UNAVAILABLE",
	"TOOL_NOT_AVAILABLE",
}

func isMissingCapabilityCode(code string) bool {
	normalized := strings.ToUpper(strings.TrimSpace(code))
	normalized = strings.NewReplacer("-", "_", " ", "_", ".", "_", ":", "_").Replace(normalized)
	for _, marker := range missingCapabilityCodeMarkers {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

// reclassifyMissingCapabilityBlock is the backstop for capability losses the
// preflight does not anticipate (#2197): a goober that discovers mid-session
// that it lacks a tool it was told it has, and reports `blocked`, is
// reclassified to `failure`. Blocked means a per-item business/content block
// and terminates the run at PhaseEscalated, which needs-human-parks every item
// the run has claimed — up to a full 20-item curate batch parked for a
// harness defect none of those items caused. As a failure it takes the
// comment-only, no-label path instead, leaving each item exactly as it was.
// The model's own narrative is preserved in the message, and blocks carrying
// any other code (DEPENDENCY_NOT_MET included) are left untouched.
func reclassifyMissingCapabilityBlock(result *apiv1.ResultEnvelope) {
	if result == nil || result.Status != apiv1.ResultBlocked || result.Error == nil {
		return
	}
	if !isMissingCapabilityCode(result.Error.Code) {
		return
	}
	authored := authoredCause(*result)
	result.Status = apiv1.ResultFailure
	if result.Outputs == nil {
		result.Outputs = map[string]interface{}{}
	}
	result.Outputs["capabilityUnsatisfied"] = true
	result.Error = &apiv1.ErrorInfo{
		Code:      ErrorCodeCapabilityUnsatisfied,
		Retryable: false,
		Message: fmt.Sprintf(
			"a missing tool capability is a system defect, not a per-item block: grant the tool to this goober "+
				"or fix the harness invocation. The agent reported: %s",
			authored),
	}
	result.Summary = "declared capability unavailable to the harness session"
}
