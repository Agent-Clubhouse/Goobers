package vcurrent

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"testing"

	"github.com/goobers/goobers/api/schemas"
	"github.com/goobers/goobers/internal/capability"
)

// This file is the single, registry-driven guard against a whole class of bug
// (#1839, first repaired one-off in #1576): a published JSON-schema enum in
// api/schemas silently drifting apart from the Go const set that is its source
// of truth. Prior to this consolidation the guard existed as three separate,
// hand-written tests — capabilities and policyActions here, journal-event
// `type` over in internal/journal — each of which had to be remembered and
// wired up by hand, so most const-backed enums were unguarded and a newly
// added enum was unguarded by default.
//
// The design has two halves:
//
//  1. constBackedEnums lists every schema enum whose members are the string
//     values of a named Go const type. TestSchemaEnumsMatchGoConsts asserts
//     bidirectional set parity for each, deriving the Go side by parsing the
//     source so the guard itself can never fall behind the taxonomy.
//
//  2. notConstBackedEnums lists every remaining schema enum, each with the
//     reason it has no Go const source of truth (boolean/filename/literal
//     domain, an untyped const group, or a structural constraint).
//     TestEverySchemaEnumIsClassified walks all schemas and fails if any enum
//     is in neither list — so a newly added enum (or a new location for an
//     existing one) cannot silently opt out of the guard.

// enumRule binds one schema enum location to the Go const set that must equal
// it. exclude, when set, returns const values that are deliberately absent from
// the schema (a documented subset), so the guard is "parity modulo declared
// exclusions", never naive equality.
type enumRule struct {
	schema  string // schema file name in api/schemas
	path    string // slash-joined JSON path to the enum array (see enumPath)
	source  string // human description of the Go source of truth
	want    func(t *testing.T) []string
	exclude func(t *testing.T) []string
}

// goConsts returns a want func that extracts every string value of const type
// typeName from the Go source file at repo-relative relPath.
func goConsts(relPath, typeName string) func(t *testing.T) []string {
	return func(t *testing.T) []string {
		return typedStringConsts(t, filepath.Join(moduleRoot(t), filepath.FromSlash(relPath)), typeName)
	}
}

func TestEnvelopeIsValidMatchesGoConsts(t *testing.T) {
	const envelope = "api/v1alpha1/envelope.go"
	srcFile := filepath.Join(moduleRoot(t), filepath.FromSlash(envelope))
	for _, typeName := range []string{"FindingClass", "ResultStatus", "VerdictDecision"} {
		t.Run(typeName, func(t *testing.T) {
			declared := typedStringConsts(t, srcFile, typeName)
			accepted := isValidStringCases(t, srcFile, typeName)
			if missing := stringsMissingFrom(declared, accepted); len(missing) > 0 {
				t.Errorf("%s.IsValid rejects declared values: %v", typeName, missing)
			}
			if extra := stringsMissingFrom(accepted, declared); len(extra) > 0 {
				t.Errorf("%s.IsValid accepts undeclared values: %v", typeName, extra)
			}
		})
	}
}

// allCapabilities is the full canonical capability registry — the want set for
// every `capabilities` enum and the mcpCredentialRef `capability` enum. The
// runner-only entries it contains are then subtracted via runnerOnlyCapabilities
// so the exclusion flows through the rule.exclude mechanism rather than being
// pre-filtered, giving that "parity modulo declared exclusions" path real
// coverage.
func allCapabilities(_ *testing.T) []string {
	out := make([]string, 0, len(capability.All()))
	for _, c := range capability.All() {
		out = append(out, string(c))
	}
	return out
}

// runnerOnlyCapabilities are the canonical capabilities a stage or goober may
// NOT declare (StageDeclarable withholds them, e.g. configrepo:read), so they
// are absent from the schema enums by design. Deriving them from StageDeclarable
// keeps the exclusion self-maintaining as the registry grows.
func runnerOnlyCapabilities(_ *testing.T) []string {
	var out []string
	for _, c := range capability.All() {
		if !capability.StageDeclarable(string(c)) {
			out = append(out, string(c))
		}
	}
	return out
}

// policyActionVocabulary is the compiler's known policy-action set — the source
// of truth for the goober/workflow policyActions and conditionalPolicyActions
// enums.
func policyActionVocabulary(_ *testing.T) []string { return knownPolicyActions() }

var constBackedEnums = []enumRule{
	// --- capabilities (4 locations, one vocabulary). The runner-only entries
	// StageDeclarable withholds are subtracted via exclude, exercising the
	// "parity modulo declared exclusions" path. ---
	{schema: "goober.schema.json", path: "properties/spec/properties/capabilities/items/enum", source: "internal/capability.All() minus runner-only", want: allCapabilities, exclude: runnerOnlyCapabilities},
	{schema: "workflow.schema.json", path: "$defs/task/properties/capabilities/items/enum", source: "internal/capability.All() minus runner-only", want: allCapabilities, exclude: runnerOnlyCapabilities},
	{schema: "invocation.schema.json", path: "properties/capabilities/items/enum", source: "internal/capability.All() minus runner-only", want: allCapabilities, exclude: runnerOnlyCapabilities},
	{schema: "goober.schema.json", path: "$defs/mcpCredentialRef/properties/capability/enum", source: "internal/capability.All() minus runner-only", want: allCapabilities, exclude: runnerOnlyCapabilities},
	{schema: "instance.schema.json", path: "$defs/credentialGrant/properties/capability/enum", source: "internal/capability.All() minus runner-only", want: allCapabilities, exclude: runnerOnlyCapabilities},

	// --- policy actions (3 locations, one vocabulary) ---
	{schema: "goober.schema.json", path: "properties/spec/properties/policyActions/items/enum", source: "compiler policy-action vocabulary", want: policyActionVocabulary},
	{schema: "goober.schema.json", path: "properties/spec/properties/conditionalPolicyActions/items/enum", source: "compiler policy-action vocabulary", want: policyActionVocabulary},
	{schema: "workflow.schema.json", path: "$defs/task/properties/policyActions/items/enum", source: "compiler policy-action vocabulary", want: policyActionVocabulary},

	// --- journal event/identity types ---
	{schema: "journal-event.schema.json", path: "properties/type/enum", source: "internal/journal.EventType", want: goConsts("internal/journal/event.go", "EventType")},
	{schema: "journal-event.schema.json", path: "properties/attemptClass/enum", source: "internal/journal.AttemptClass", want: goConsts("internal/journal/event.go", "AttemptClass")},
	{schema: "journal-event.schema.json", path: "properties/branchStatus/enum", source: "internal/journal.BranchStatus", want: goConsts("internal/journal/event.go", "BranchStatus")},
	{schema: "journal-event.schema.json", path: "properties/completeness/items/properties/status/enum", source: "internal/journal.BranchStatus", want: goConsts("internal/journal/event.go", "BranchStatus")},
	{schema: "journal-run.schema.json", path: "properties/trigger/properties/kind/enum", source: "internal/journal.TriggerKind", want: goConsts("internal/journal/identity.go", "TriggerKind")},

	// --- api/v1alpha1 envelope types ---
	{schema: "result.schema.json", path: "properties/status/enum", source: "api/v1alpha1.ResultStatus", want: goConsts("api/v1alpha1/envelope.go", "ResultStatus")},
	{schema: "verdict.schema.json", path: "properties/decision/enum", source: "api/v1alpha1.VerdictDecision", want: goConsts("api/v1alpha1/envelope.go", "VerdictDecision")},
	{schema: "verdict.schema.json", path: "$defs/finding/properties/severity/enum", source: "api/v1alpha1.Severity", want: goConsts("api/v1alpha1/envelope.go", "Severity")},
	{schema: "verdict.schema.json", path: "$defs/finding/properties/class/enum", source: "api/v1alpha1.FindingClass", want: goConsts("api/v1alpha1/envelope.go", "FindingClass")},
	{schema: "notification-request.schema.json", path: "properties/severity/enum", source: "api/v1alpha1.NotificationSeverity", want: goConsts("api/v1alpha1/notification.go", "NotificationSeverity")},
	{schema: "notification-receipt.schema.json", path: "properties/status/enum", source: "api/v1alpha1.NotificationDeliveryStatus", want: goConsts("api/v1alpha1/notification.go", "NotificationDeliveryStatus")},

	// --- mission-control verdict artifact types ---
	{schema: "mission-control-verdict-v1alpha1.schema.json", path: "$defs/verdict/enum", source: "internal/missioncontrol.Verdict", want: goConsts("internal/missioncontrol/contract.go", "Verdict")},
	{schema: "mission-control-verdict-v1alpha1.schema.json", path: "$defs/reasonCode/enum", source: "internal/missioncontrol.ReasonCode", want: goConsts("internal/missioncontrol/contract.go", "ReasonCode")},
	{schema: "mission-control-verdict-v1alpha1.schema.json", path: "$defs/requirement/enum", source: "internal/missioncontrol.Requirement", want: goConsts("internal/missioncontrol/contract.go", "Requirement")},
	{schema: "mission-control-verdict-v1alpha1.schema.json", path: "$defs/policy/properties/unknown/enum", source: "internal/missioncontrol.UnknownPolicy", want: goConsts("internal/missioncontrol/contract.go", "UnknownPolicy")},
	{schema: "mission-control-verdict-v1alpha1.schema.json", path: "$defs/criterion/properties/comparator/enum", source: "internal/missioncontrol.Comparator", want: goConsts("internal/missioncontrol/contract.go", "Comparator")},
	{schema: "mission-control-verdict-v1alpha1.schema.json", path: "$defs/value/properties/type/enum", source: "internal/missioncontrol.ValueType", want: goConsts("internal/missioncontrol/contract.go", "ValueType")},

	// --- input-integrity grades ---
	{schema: "artifact-pointer.schema.json", path: "properties/integrity/enum", source: "api/integrity.Grade", want: goConsts("api/integrity/grade.go", "Grade")},
	{schema: "invocation.schema.json", path: "$defs/contextPointer/properties/integrity/enum", source: "api/integrity.Grade", want: goConsts("api/integrity/grade.go", "Grade")},
	{schema: "invocation.schema.json", path: "properties/minimumIntegrity/enum", source: "api/integrity.Grade", want: goConsts("api/integrity/grade.go", "Grade")},
	{schema: "invocation.schema.json", path: "$defs/backlogItem/properties/integrity/enum", source: "api/integrity.Grade", want: goConsts("api/integrity/grade.go", "Grade")},
	{schema: "journal-event.schema.json", path: "properties/integrity/enum", source: "api/integrity.Grade", want: goConsts("api/integrity/grade.go", "Grade")},
	{schema: "result.schema.json", path: "properties/integrity/enum", source: "api/integrity.Grade", want: goConsts("api/integrity/grade.go", "Grade")},
	{schema: "journal-event.schema.json", path: "$defs/ref/properties/integrity/enum", source: "api/integrity.Grade", want: goConsts("api/integrity/grade.go", "Grade")},
	{schema: "journal-event.schema.json", path: "properties/minimumIntegrity/enum", source: "api/integrity.Grade", want: goConsts("api/integrity/grade.go", "Grade")},
	{schema: "journal-run.schema.json", path: "properties/inputs/items/properties/integrity/enum", source: "api/integrity.Grade", want: goConsts("api/integrity/grade.go", "Grade")},
	{schema: "journal-run.schema.json", path: "properties/inputs/items/properties/ref/properties/integrity/enum", source: "api/integrity.Grade", want: goConsts("api/integrity/grade.go", "Grade")},
	{schema: "remediation-brief-v3.schema.json", path: "$defs/integrity/enum", source: "api/integrity.Grade", want: goConsts("api/integrity/grade.go", "Grade")},
	{schema: "workflow.schema.json", path: "$defs/task/properties/minimumIntegrity/enum", source: "api/integrity.Grade", want: goConsts("api/integrity/grade.go", "Grade")},

	// --- api/v1alpha1 workflow types ---
	{schema: "workflow.schema.json", path: "$defs/task/properties/type/enum", source: "api/v1alpha1.TaskType", want: goConsts("api/v1alpha1/workflow_types.go", "TaskType")},
	{schema: "workflow.schema.json", path: "$defs/gate/properties/evaluator/enum", source: "api/v1alpha1.EvaluatorKind", want: goConsts("api/v1alpha1/workflow_types.go", "EvaluatorKind")},
	{schema: "workflow.schema.json", path: "$defs/trigger/properties/type/enum", source: "api/v1alpha1.TriggerType", want: goConsts("api/v1alpha1/workflow_types.go", "TriggerType")},
	{schema: "workflow.schema.json", path: "$defs/task/properties/run/properties/workspace/enum", source: "api/v1alpha1.WorkspaceMode", want: goConsts("api/v1alpha1/workflow_types.go", "WorkspaceMode")},
	{schema: "workflow.schema.json", path: "$defs/task/properties/workspace/enum", source: "api/v1alpha1.WorkspaceMode", want: goConsts("api/v1alpha1/workflow_types.go", "WorkspaceMode")},
	{schema: "workflow.schema.json", path: "$defs/gate/properties/agentic/properties/workspace/enum", source: "api/v1alpha1.WorkspaceMode", want: goConsts("api/v1alpha1/workflow_types.go", "WorkspaceMode")},
	{schema: "workflow.schema.json", path: "$defs/parallel/properties/failurePolicy/enum", source: "api/v1alpha1.BranchFailurePolicy", want: goConsts("api/v1alpha1/workflow_types.go", "BranchFailurePolicy")},
	{schema: "workflow.schema.json", path: "properties/spec/properties/tutorScope/properties/tier/enum", source: "api/v1alpha1.TutorScopeTier", want: goConsts("api/v1alpha1/workflow_types.go", "TutorScopeTier")},

	// --- api/v1alpha1 manifest / goober / common types ---
	{schema: "manifest.schema.json", path: "properties/spec/properties/instance/properties/environment/enum", source: "api/v1alpha1.Environment", want: goConsts("api/v1alpha1/manifest_types.go", "Environment")},
	{schema: "goober.schema.json", path: "properties/spec/properties/harness/enum", source: "api/v1alpha1.Harness", want: goConsts("api/v1alpha1/goober_types.go", "Harness")},
	{schema: "goober.schema.json", path: "$defs/mcpCredentialRef/properties/scheme/enum", source: "api/v1alpha1.MCPHeaderScheme", want: goConsts("api/v1alpha1/goober_types.go", "MCPHeaderScheme")},
	{schema: "gaggle.schema.json", path: "$defs/repoRef/properties/provider/enum", source: "api/v1alpha1.Provider", want: goConsts("api/v1alpha1/common.go", "Provider")},
	{schema: "gaggle.schema.json", path: "$defs/backlogRef/properties/provider/enum", source: "api/v1alpha1.Provider", want: goConsts("api/v1alpha1/common.go", "Provider")},
	{schema: "invocation.schema.json", path: "$defs/backlogItem/properties/provider/enum", source: "api/v1alpha1.Provider", want: goConsts("api/v1alpha1/common.go", "Provider")},
	{schema: "invocation.schema.json", path: "$defs/repoRef/properties/provider/enum", source: "api/v1alpha1.Provider", want: goConsts("api/v1alpha1/common.go", "Provider")},
	{schema: "instance.schema.json", path: "$defs/repo/properties/provider/enum", source: "api/v1alpha1.Provider", want: goConsts("api/v1alpha1/common.go", "Provider")},
	{schema: "instance.schema.json", path: "$defs/harnessName/enum", source: "api/v1alpha1.Harness", want: goConsts("api/v1alpha1/goober_types.go", "Harness")},

	// --- feature / support-matrix / sandbox types ---
	{schema: "features.schema.json", path: "properties/features/items/properties/stability/enum", source: "internal/workflow/v_current.SupportLevel", want: goConsts("internal/workflow/v_current/features.go", "SupportLevel")},
	{schema: "agent-toolkit-manifest.schema.json", path: "$defs/dslVersion/properties/level/enum", source: "internal/supportmatrix.Level", want: goConsts("internal/supportmatrix/supportmatrix.go", "Level")},
	{schema: "agent-toolkit-manifest.schema.json", path: "$defs/dslVersion/properties/history/items/properties/level/enum", source: "internal/supportmatrix.Level", want: goConsts("internal/supportmatrix/supportmatrix.go", "Level")},
	{schema: "gaggle.schema.json", path: "properties/spec/properties/sandbox/properties/agentic/enum", source: "internal/instance.SandboxPosture", want: goConsts("internal/instance/sandbox.go", "SandboxPosture")},
	{schema: "instance.schema.json", path: "properties/sandbox/properties/agentic/enum", source: "internal/instance.SandboxPosture", want: goConsts("internal/instance/sandbox.go", "SandboxPosture")},
}

// notConstBackedEnums documents every schema enum that has no named Go const
// source of truth, so completeness can be enforced without silently ignoring
// anything. Each value is the reason it is not const-backed.
var notConstBackedEnums = map[string]string{
	"explain.schema.json\x00properties/stability/enum":                                              "lifecycle values are drawn from two sources with no shared Go type: only \"ga\" has a const (untyped schemas.StabilityGA), and preview/deprecated/removed come from the DSL feature registry; internal/authoring.Explanation.Stability is a plain string",
	"workflow.schema.json\x00$defs/task/properties/onTimeout/enum":                                  "backing consts TaskOnTimeoutFail/Salvage are untyped string constants — no named enum type to bind",
	"workflow.schema.json\x00$defs/gate/properties/human/properties/onTimeout/enum":                 "human-gate timeout actions are schema-only string literals; no Go const source",
	"workflow.schema.json\x00$defs/task/properties/run/properties/network/enum":                     "single 'none' sentinel; not a Go enum",
	"workflow.schema.json\x00$defs/task/properties/run/allOf/0/not/properties/workspace/enum":       "structural exclusion constraint (a subset of WorkspaceMode), not a value domain",
	"manifest.schema.json\x00$defs/connection/properties/type/enum":                                 "connection kinds are schema-only string literals; no Go typed const",
	"instance-repository-execution.schema.json\x00properties/workspace/properties/cleanPolicy/enum": "backing workspace clean-policy constants are untyped strings",
	"instance.schema.json\x00$defs/repoAuth/properties/kind/enum":                                   "repository auth kinds are untyped string constants (internal/instance ADOAuth*/GitHubAuth*), and the schema enum is the UNION of two provider-specific sets no single Go type expresses",
	"instance.schema.json\x00$defs/daemonIdentity/properties/kind/enum":                             "daemon identity kinds reuse the same untyped GitHubAuthPAT/GitHubAuthApp constants; DaemonIdentityConfig.Kind is deliberately an open string, not a named enum type",
	"instance.schema.json\x00$defs/secretStore/properties/kind/enum":                                "secret store vendor kinds are untyped string constants (internal/instance.SecretStoreKind*)",
	"instance.schema.json\x00$defs/secretStore/properties/auth/properties/kind/enum":                "secret store ambient-auth kinds are untyped string constants (internal/instance.SecretStoreAuth*)",
	"instance.schema.json\x00$defs/workflowSource/properties/kind/enum":                             "workflow source kinds are untyped string constants (internal/instance.WorkflowSourceKind*)",
	"instance.schema.json\x00$defs/repoPolicy/properties/requiredMergeMethod/enum":                  "merge methods are schema-only string literals compared inline in instance config validation; no Go const source",
	"instance.schema.json\x00$defs/speech/properties/engine/enum":                                   "speech engines are untyped string constants (internal/speechnotify.Engine*)",
	"instance.schema.json\x00$defs/externalTelemetryConnector/properties/auth/properties/mode/enum": "external telemetry auth modes are untyped string constants (internal/externaltelemetry.Auth*)",
	"config-source-action.schema.json\x00properties/action/enum":                                    "onboarding action names are schema-only string literals",
	"candidate-findings-v1.schema.json\x00$defs/finding/properties/kind/enum":                       "tutor finding kinds are compared as bare string literals (internal/tutorguard); no typed const",
	"diagnostics.schema.json\x00$defs/finding/properties/severity/enum":                             "error/warning is duplicated across several Severity types (configdiff/validate/v1alpha1); no single canonical source to bind",
	"agent-toolkit-manifest.schema.json\x00$defs/adapter/properties/harness/enum":                   "agentkit harness ids are string literals, a distinct set from api/v1alpha1.Harness",
	"agent-toolkit-manifest.schema.json\x00$defs/adapter/properties/instructionTarget/enum":         "instruction filenames, not a Go enum",
	"remediation-brief-v1.schema.json\x00properties/hasSubstantiveFindings/enum":                    "boolean literal domain [true,false]",
	"remediation-brief-v1.schema.json\x00properties/hasFailingCI/enum":                              "boolean literal domain [true,false]",
	"remediation-brief-v2.schema.json\x00properties/hasSubstantiveFindings/enum":                    "boolean literal domain [true,false]",
	"remediation-brief-v2.schema.json\x00properties/hasFailingCI/enum":                              "boolean literal domain [true,false]",
	"remediation-brief-v3.schema.json\x00properties/hasSubstantiveFindings/enum":                    "boolean literal domain [true,false]",
	"remediation-brief-v3.schema.json\x00properties/hasFailingCI/enum":                              "boolean literal domain [true,false]",
}

func TestSchemaEnumsMatchGoConsts(t *testing.T) {
	for _, rule := range constBackedEnums {
		rule := rule
		t.Run(rule.schema+" "+rule.path, func(t *testing.T) {
			enums := collectSchemaEnums(t, rule.schema)
			raw, ok := enums[rule.path]
			if !ok {
				t.Fatalf("schema %s has no enum at %s (registry is stale)", rule.schema, rule.path)
			}
			got := enumStrings(t, raw, rule.schema+" "+rule.path)

			want := rule.want(t)
			if len(want) == 0 {
				t.Fatalf("no Go consts found for %s — wrong source/type mapping", rule.source)
			}
			var excluded []string
			if rule.exclude != nil {
				excluded = rule.exclude(t)
			}
			exclude := make(map[string]bool, len(excluded))
			for _, e := range excluded {
				exclude[e] = true
			}
			var filtered []string
			for _, w := range want {
				if !exclude[w] {
					filtered = append(filtered, w)
				}
			}

			if diff := setDiff(filtered, got); diff != "" {
				t.Fatalf("enum at %s drifted from %s (parity modulo exclusions %v):\n%s",
					rule.path, rule.source, excluded, diff)
			}
		})
	}
}

func TestEverySchemaEnumIsClassified(t *testing.T) {
	registered := make(map[string]bool)
	for _, rule := range constBackedEnums {
		registered[rule.schema+"\x00"+rule.path] = true
	}

	entries, err := schemas.FS.ReadDir(".")
	if err != nil {
		t.Fatalf("read schemas dir: %v", err)
	}
	seen := make(map[string]bool)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".json" {
			continue
		}
		for path := range collectSchemaEnums(t, name) {
			key := name + "\x00" + path
			seen[key] = true
			if !registered[key] {
				if _, isLiteral := notConstBackedEnums[key]; !isLiteral {
					t.Errorf("unclassified schema enum %s at %s: register it in constBackedEnums (with its Go const type) or, if it has no Go const source, add it to notConstBackedEnums with a reason", name, path)
				}
			}
		}
	}

	// A registry entry naming an enum that no longer exists is stale and must
	// be removed, or the guard silently protects nothing.
	for key := range registered {
		if !seen[key] {
			t.Errorf("constBackedEnums entry %q refers to an enum that no longer exists in the schema", humanKey(key))
		}
	}
	for key := range notConstBackedEnums {
		if !seen[key] {
			t.Errorf("notConstBackedEnums entry %q refers to an enum that no longer exists in the schema", humanKey(key))
		}
	}
}

func humanKey(key string) string {
	for i := 0; i < len(key); i++ {
		if key[i] == 0 {
			return key[:i] + " " + key[i+1:]
		}
	}
	return key
}

// typedStringConsts parses a Go source file and returns every string value of a
// const explicitly typed as typeName. Requiring an explicit type on the spec
// keeps it from collecting unrelated untyped constants.
func typedStringConsts(t *testing.T, srcFile, typeName string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, srcFile, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", srcFile, err)
	}
	var out []string
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			id, ok := vs.Type.(*ast.Ident)
			if !ok || id.Name != typeName {
				continue
			}
			for _, value := range vs.Values {
				lit, ok := value.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				unquoted, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s value: %v", typeName, err)
				}
				out = append(out, unquoted)
			}
		}
	}
	return out
}

// isValidStringCases resolves every case that returns true from typeName.IsValid.
// It deliberately fails on a different method shape rather than silently deriving
// an incomplete accepted set.
func isValidStringCases(t *testing.T, srcFile, typeName string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, srcFile, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", srcFile, err)
	}

	constValues := make(map[string]string)
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			id, ok := vs.Type.(*ast.Ident)
			if !ok || id.Name != typeName || len(vs.Names) != len(vs.Values) {
				continue
			}
			for i, expr := range vs.Values {
				lit, ok := expr.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s value: %v", typeName, err)
				}
				constValues[vs.Names[i].Name] = value
			}
		}
	}

	var method *ast.FuncDecl
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "IsValid" || fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}
		recv, ok := fn.Recv.List[0].Type.(*ast.Ident)
		if ok && recv.Name == typeName {
			method = fn
			break
		}
	}
	if method == nil {
		t.Fatalf("%s.IsValid method not found", typeName)
	}

	if len(method.Body.List) != 2 {
		t.Fatalf("%s.IsValid must contain a switch followed by return false", typeName)
	}
	switchStmt, ok := method.Body.List[0].(*ast.SwitchStmt)
	if !ok || !returnsBool(method.Body.List[1], false) {
		t.Fatalf("%s.IsValid must contain a switch followed by return false", typeName)
	}
	receiver := method.Recv.List[0]
	if len(receiver.Names) != 1 {
		t.Fatalf("%s.IsValid receiver must be named", typeName)
	}
	tag, ok := switchStmt.Tag.(*ast.Ident)
	if !ok || tag.Name != receiver.Names[0].Name {
		t.Fatalf("%s.IsValid must switch directly on its receiver", typeName)
	}

	var out []string
	for _, stmt := range switchStmt.Body.List {
		clause, ok := stmt.(*ast.CaseClause)
		if !ok {
			t.Fatalf("%s.IsValid switch contains %T, want case clause", typeName, stmt)
		}
		if len(clause.List) == 0 {
			if len(clause.Body) != 1 || !returnsBool(clause.Body[0], false) {
				t.Fatalf("%s.IsValid default case must directly return false", typeName)
			}
			continue
		}
		if len(clause.Body) != 1 || !returnsBool(clause.Body[0], true) {
			t.Fatalf("%s.IsValid has a non-default case that does not directly return true", typeName)
		}
		for _, expr := range clause.List {
			switch value := expr.(type) {
			case *ast.Ident:
				resolved, ok := constValues[value.Name]
				if !ok {
					t.Fatalf("%s.IsValid case %s is not a declared %s const", typeName, value.Name, typeName)
				}
				out = append(out, resolved)
			case *ast.BasicLit:
				if value.Kind != token.STRING {
					t.Fatalf("%s.IsValid has a non-string case", typeName)
				}
				resolved, err := strconv.Unquote(value.Value)
				if err != nil {
					t.Fatalf("unquote %s.IsValid case: %v", typeName, err)
				}
				out = append(out, resolved)
			default:
				t.Fatalf("%s.IsValid has an unsupported case expression %T", typeName, expr)
			}
		}
	}
	return out
}

func returnsBool(stmt ast.Stmt, want bool) bool {
	ret, ok := stmt.(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return false
	}
	id, ok := ret.Results[0].(*ast.Ident)
	return ok && id.Name == strconv.FormatBool(want)
}

func stringsMissingFrom(values, other []string) []string {
	otherSet := make(map[string]bool, len(other))
	for _, value := range other {
		otherSet[value] = true
	}
	var missing []string
	for _, value := range values {
		if !otherSet[value] {
			missing = append(missing, value)
		}
	}
	sort.Strings(missing)
	return missing
}

// collectSchemaEnums decodes a schema and returns every enum array keyed by its
// slash-joined JSON path (array indices included). It fails on a duplicate
// entry within any enum — the second half of the #1576 defect.
func collectSchemaEnums(t *testing.T, schemaName string) map[string][]any {
	t.Helper()
	data, err := schemas.FS.ReadFile(schemaName)
	if err != nil {
		t.Fatalf("read schema %s: %v", schemaName, err)
	}
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode schema %s: %v", schemaName, err)
	}
	out := make(map[string][]any)
	walkEnums(t, schemaName, doc, "", out)
	return out
}

func walkEnums(t *testing.T, schemaName string, node any, path string, out map[string][]any) {
	switch n := node.(type) {
	case map[string]any:
		for key, value := range n {
			child := key
			if path != "" {
				child = path + "/" + key
			}
			if key == "enum" {
				if arr, ok := value.([]any); ok {
					seen := make(map[string]bool, len(arr))
					for _, v := range arr {
						if s, ok := v.(string); ok {
							if seen[s] {
								t.Errorf("%s enum at %s lists %q more than once", schemaName, child, s)
							}
							seen[s] = true
						}
					}
					out[child] = arr
					continue
				}
			}
			walkEnums(t, schemaName, value, child, out)
		}
	case []any:
		for i, value := range n {
			walkEnums(t, schemaName, value, path+"/"+strconv.Itoa(i), out)
		}
	}
}

func enumStrings(t *testing.T, arr []any, where string) []string {
	t.Helper()
	out := make([]string, 0, len(arr))
	for i, v := range arr {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("%s enum[%d] is not a string: %v", where, i, v)
		}
		out = append(out, s)
	}
	return out
}

// setDiff compares two string collections as sets and returns a human-readable
// description of the difference, or "" when they are equal.
func setDiff(want, got []string) string {
	wantSet := make(map[string]bool, len(want))
	for _, w := range want {
		wantSet[w] = true
	}
	gotSet := make(map[string]bool, len(got))
	for _, g := range got {
		gotSet[g] = true
	}
	var missing, extra []string
	for w := range wantSet {
		if !gotSet[w] {
			missing = append(missing, w)
		}
	}
	for g := range gotSet {
		if !wantSet[g] {
			extra = append(extra, g)
		}
	}
	if len(missing) == 0 && len(extra) == 0 {
		return ""
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return "  in Go const set but missing from schema enum: " + fmtList(missing) + "\n" +
		"  in schema enum but not a Go const: " + fmtList(extra)
}

func fmtList(s []string) string {
	if len(s) == 0 {
		return "(none)"
	}
	return "[" + join(s, ", ") + "]"
}

func join(s []string, sep string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += sep
		}
		out += v
	}
	return out
}

// moduleRoot walks up from this test file to the directory holding go.mod, so
// the AST extractor can locate source files in sibling packages regardless of
// the working directory the test runs from.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above test file")
		}
		dir = parent
	}
}
