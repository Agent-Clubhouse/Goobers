package vnext

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/goobers/goobers/api/schemas"
)

// authorSchemaFiles are the embedded schemas whose leaves the feature registry
// must cover. The gaggle and goober surfaces are separate documents — their
// leaves are NOT in workflow.schema.json — so all three must be walked.
var authorSchemaFiles = map[string]string{
	"workflow": "workflow.schema.json",
	"gaggle":   "gaggle.schema.json",
	"goober":   "goober.schema.json",
}

// schemaLeafExceptions lists every author-facing schema leaf that resolves to
// no registered FeatureID, each with a settled reason. The #3292 backfill
// discharged the worklist this map used to carry: every leaf that earns its
// own FeatureID under the naming rules is registered, and what remains here
// is deliberate — identity envelope, per-enum-value discriminators, payload
// folded into a registered container feature, and id spellings that differ
// from the leaf path. Adding a leaf to any of the three schemas without
// either a FeatureID or an entry here fails
// TestEmbeddedSchemaLeavesMapToFeatureRegistry; entries that become
// registered (or leave the schemas) fail it too.
var schemaLeafExceptions = map[string]string{
	// Identity envelope: fixed document identity and k8s-style metadata are not
	// DSL capabilities and are expected to stay excepted.
	"gaggle.apiVersion":             "identity envelope: fixed document identity, not a DSL capability",
	"gaggle.kind":                   "identity envelope: fixed document identity, not a DSL capability",
	"gaggle.metadata.annotations":   "identity envelope: k8s-style object metadata, not a DSL capability",
	"gaggle.metadata.labels":        "identity envelope: k8s-style object metadata, not a DSL capability",
	"gaggle.metadata.name":          "identity envelope: k8s-style object metadata, not a DSL capability",
	"goober.apiVersion":             "identity envelope: fixed document identity, not a DSL capability",
	"goober.kind":                   "identity envelope: fixed document identity, not a DSL capability",
	"goober.metadata.annotations":   "identity envelope: k8s-style object metadata, not a DSL capability",
	"goober.metadata.labels":        "identity envelope: k8s-style object metadata, not a DSL capability",
	"goober.metadata.name":          "identity envelope: k8s-style object metadata, not a DSL capability",
	"workflow.apiVersion":           "identity envelope: fixed document identity, not a DSL capability",
	"workflow.kind":                 "identity envelope: fixed document identity, not a DSL capability",
	"workflow.metadata.annotations": "identity envelope: k8s-style object metadata, not a DSL capability",
	"workflow.metadata.labels":      "identity envelope: k8s-style object metadata, not a DSL capability",
	"workflow.metadata.name":        "identity envelope: k8s-style object metadata, not a DSL capability",
	"workflow.dslVersion":           "identity envelope: selects the interpreting DSL version; versions are the support matrix's surface, not a feature of it",

	// DSL 3.0-only surface (dsl-3.0.md §2/§4, issue #3505): the schemas are
	// shared across interpreters, so these leaves are parseable here, but the
	// fields exist only in 3.0 — this frozen interpreter never learns them
	// (PO-D0), and the version router refuses a 2.0 document that uses them
	// (internal/workflow preV30SurfaceProblems). They register in the 3.0
	// registry (internal/workflow/v_3_0).
	"workflow.spec.tasks.runsOn.os":           "DSL 3.0-only surface; registered as task.runsOn.os in the 3.0 registry, refused on 2.0 by the version router",
	"workflow.spec.tasks.runsOn.cpu":          "DSL 3.0-only surface; registered as task.runsOn.cpu in the 3.0 registry, refused on 2.0 by the version router",
	"workflow.spec.tasks.runsOn.memory":       "DSL 3.0-only surface; registered as task.runsOn.memory in the 3.0 registry, refused on 2.0 by the version router",
	"workflow.spec.tasks.runsOn.disk":         "DSL 3.0-only surface; registered as task.runsOn.disk in the 3.0 registry, refused on 2.0 by the version router",
	"workflow.spec.tasks.runsOn.capabilities": "DSL 3.0-only surface; registered as task.runsOn.capabilities in the 3.0 registry, refused on 2.0 by the version router",
	"workflow.spec.tasks.runsOn.restrictions": "DSL 3.0-only surface; registered as task.runsOn.restrictions in the 3.0 registry, refused on 2.0 by the version router",
	"workflow.spec.tasks.repoFrom":            "DSL 3.0-only surface; registered as task.repoFrom in the 3.0 registry, refused on 2.0 by the version router",
	"workflow.spec.tasks.commitsRepo":         "DSL 3.0-only surface; registered as task.commitsRepo in the 3.0 registry, refused on 2.0 by the version router",
	"gaggle.spec.runsOn.os":                   "DSL 3.0-only surface; registered as gaggle.spec.runsOn.os in the 3.0 registry, refused with 2.0-pinned workflows by the version router",
	"gaggle.spec.runsOn.capabilities":         "DSL 3.0-only surface; registered as gaggle.spec.runsOn.capabilities in the 3.0 registry, refused with 2.0-pinned workflows by the version router",
	"gaggle.spec.runsOn.restrictions":         "DSL 3.0-only surface; registered as gaggle.spec.runsOn.restrictions in the 3.0 registry, refused with 2.0-pinned workflows by the version router",

	// Registered under a FeatureID whose spelling differs from the leaf path.
	// #3292 ruled: keep the exception rather than align. Released FeatureIDs
	// are pinned by validateFeatureRegistryEvolution, so the id cannot be
	// respelled, and aliasing the candidate mapping here would diverge from
	// internal/authoring's selectorFeatureCandidates, which deliberately has
	// no such aliases.
	"workflow.spec.gates.agentic.retry.backoffSeconds":   "registered as gate.evaluator.agentic.retry.backoff; released ids are pinned, and the candidate mapping mirrors authoring's un-aliased rules",
	"workflow.spec.gates.automated.retry.backoffSeconds": "registered as gate.evaluator.automated.retry.backoff; released ids are pinned, and the candidate mapping mirrors authoring's un-aliased rules",
	"workflow.spec.gates.human.timeoutSeconds":           "registered as gate.evaluator.human.timeout; released ids are pinned, and the candidate mapping mirrors authoring's un-aliased rules",
	"workflow.spec.tasks.retry.backoffSeconds":           "registered as task.retry.backoff; released ids are pinned, and the candidate mapping mirrors authoring's un-aliased rules",
	"workflow.spec.triggers.selector":                    "registered as trigger.backlog-item.selector; released ids are pinned, and the candidate mapping mirrors authoring's un-aliased rules",
	"workflow.spec.triggers.trustLabel":                  "registered as trigger.backlog-item.trustLabel; released ids are pinned, and the candidate mapping mirrors authoring's un-aliased rules",

	// Discriminators registered per enum value rather than per field.
	"workflow.spec.tasks.type":    "registered per enum value (task.deterministic, task.agentic); the discriminator has no id of its own",
	"workflow.spec.triggers.type": "registered per enum value (trigger.manual, trigger.backlog-item, ...); the discriminator has no id of its own",

	// Sub-fields folded into a registered container feature (#3292 naming rule:
	// behavior-selecting fields get an ID, inert payload folds into the parent).
	"gaggle.spec.sandbox.agentic":                      "sole leaf of registered gaggle.spec.sandbox, whose released id is pinned at the container",
	"workflow.spec.parallels.branches.name":            "payload of registered workflow.spec.parallels.branches",
	"workflow.spec.parallels.branches.start":           "payload of registered workflow.spec.parallels.branches",
	"workflow.spec.parallels.name":                     "state identity payload of registered workflow.spec.parallels",
	"goober.spec.mcpServers.args":                      "payload of registered goober.spec.mcpServers (one id over the server list)",
	"goober.spec.mcpServers.command":                   "payload of registered goober.spec.mcpServers (one id over the server list)",
	"goober.spec.mcpServers.credentialRefs.capability": "payload of registered goober.spec.mcpServers (one id over the server list)",
	"goober.spec.mcpServers.credentialRefs.env":        "payload of registered goober.spec.mcpServers (one id over the server list)",
	"goober.spec.mcpServers.credentialRefs.header":     "payload of registered goober.spec.mcpServers (one id over the server list)",
	"goober.spec.mcpServers.credentialRefs.kind":       "payload of registered goober.spec.mcpServers (one id over the server list)",
	"goober.spec.mcpServers.credentialRefs.ref":        "payload of registered goober.spec.mcpServers (one id over the server list)",
	"goober.spec.mcpServers.credentialRefs.scheme":     "payload of registered goober.spec.mcpServers (one id over the server list)",
	"goober.spec.mcpServers.name":                      "payload of registered goober.spec.mcpServers (one id over the server list)",
	"goober.spec.mcpServers.url":                       "payload of registered goober.spec.mcpServers (one id over the server list)",

	// A trigger type's required payload belongs to the type feature (#3292
	// ruling): Trigger.Events is MinItems=1 and meaningless without webhook,
	// exactly as schedule/signal fold into their type ids. Only optional
	// refinements split (trigger.backlog-item.selector, trigger.idleBackoff).
	"workflow.spec.triggers.events": "required payload of registered trigger.webhook; a type's required payload folds into the type feature",

	// Identity coordinates folded into their registered container feature
	// (#3292 rule 5): owner/project/name/branch/connectionRef are inert
	// identity payload; provider, baseUrl, and checkout.sparse select behavior
	// and carry their own ids. ConnectionRef additionally has the runtime
	// credential-scoping defect tracked by #3296 — registering it would
	// promise a semantic the platform does not keep.
	"gaggle.spec.additionalRepos.branch":        "identity payload of registered gaggle.spec.additionalRepos",
	"gaggle.spec.additionalRepos.connectionRef": "identity payload of registered gaggle.spec.additionalRepos; runtime scoping defect tracked by #3296",
	"gaggle.spec.additionalRepos.name":          "identity payload of registered gaggle.spec.additionalRepos",
	"gaggle.spec.additionalRepos.owner":         "identity payload of registered gaggle.spec.additionalRepos",
	"gaggle.spec.additionalRepos.project":       "identity payload of registered gaggle.spec.additionalRepos",
	"gaggle.spec.backlog.connectionRef":         "identity payload of registered gaggle.spec.backlog; runtime scoping defect tracked by #3296",
	"gaggle.spec.backlog.project":               "identity payload of registered gaggle.spec.backlog",
	"gaggle.spec.project.branch":                "identity payload of registered gaggle.spec.project",
	"gaggle.spec.project.connectionRef":         "identity payload of registered gaggle.spec.project; runtime scoping defect tracked by #3296",
	"gaggle.spec.project.name":                  "identity payload of registered gaggle.spec.project",
	"gaggle.spec.project.owner":                 "identity payload of registered gaggle.spec.project",
	"gaggle.spec.project.project":               "identity payload of registered gaggle.spec.project",

	// Validate-only sibling metadata: gaggle.spec.siblings is one id over the
	// whole element list (#3292 rule 5); nothing at runtime reads a sibling.
	"gaggle.spec.siblings.label":                   "payload of registered gaggle.spec.siblings (one id over the sibling list)",
	"gaggle.spec.siblings.project.baseUrl":         "payload of registered gaggle.spec.siblings (one id over the sibling list)",
	"gaggle.spec.siblings.project.branch":          "payload of registered gaggle.spec.siblings (one id over the sibling list)",
	"gaggle.spec.siblings.project.checkout.sparse": "payload of registered gaggle.spec.siblings (one id over the sibling list)",
	"gaggle.spec.siblings.project.connectionRef":   "payload of registered gaggle.spec.siblings (one id over the sibling list)",
	"gaggle.spec.siblings.project.name":            "payload of registered gaggle.spec.siblings (one id over the sibling list)",
	"gaggle.spec.siblings.project.owner":           "payload of registered gaggle.spec.siblings (one id over the sibling list)",
	"gaggle.spec.siblings.project.project":         "payload of registered gaggle.spec.siblings (one id over the sibling list)",
	"gaggle.spec.siblings.project.provider":        "payload of registered gaggle.spec.siblings (one id over the sibling list)",
	"gaggle.spec.siblings.requireLabels":           "payload of registered gaggle.spec.siblings (one id over the sibling list)",
}

// TestEmbeddedSchemaLeavesMapToFeatureRegistry is the fail-closed schema-to-
// registry guard (#3292): it derives the author-facing leaf set from the
// embedded workflow/gaggle/goober schemas and requires each leaf to resolve to
// a registered FeatureID or carry a reasoned exception above.
func TestEmbeddedSchemaLeavesMapToFeatureRegistry(t *testing.T) {
	violations := schemaLeafRegistryViolations(t, loadAuthorSchemas(t), schemaLeafExceptions)
	if len(violations) > 0 {
		t.Fatalf(
			"embedded schema leaves and the feature registry disagree (#3292):\n  %s",
			strings.Join(violations, "\n  "),
		)
	}
}

// TestSchemaLeafGuardFires proves the guard actually rejects each failing
// direction: a new unmapped leaf, a stale exception for a vanished leaf, and
// an obsolete exception for a leaf that gained a FeatureID.
func TestSchemaLeafGuardFires(t *testing.T) {
	documents := map[string]map[string]any{
		"workflow": {
			"properties": map[string]any{
				"spec": map[string]any{
					"properties": map[string]any{
						"gaggle":       map[string]any{"type": "string"},
						"shinyNewKnob": map[string]any{"type": "string"},
					},
				},
			},
		},
	}

	t.Run("unmapped leaf", func(t *testing.T) {
		violations := schemaLeafRegistryViolations(t, documents, nil)
		wantViolation(t, violations, `leaf "workflow.spec.shinyNewKnob" has no registered FeatureID`)
	})
	t.Run("reasonless exception", func(t *testing.T) {
		violations := schemaLeafRegistryViolations(t, documents, map[string]string{
			"workflow.spec.shinyNewKnob": "  ",
		})
		wantViolation(t, violations, `exception for "workflow.spec.shinyNewKnob" has no reason`)
	})
	t.Run("stale exception", func(t *testing.T) {
		violations := schemaLeafRegistryViolations(t, documents, map[string]string{
			"workflow.spec.shinyNewKnob": "pending backfill",
			"workflow.spec.vanished":     "no longer a schema leaf",
		})
		wantViolation(t, violations, `exception for "workflow.spec.vanished" names no current schema leaf`)
	})
	t.Run("obsolete exception", func(t *testing.T) {
		violations := schemaLeafRegistryViolations(t, documents, map[string]string{
			"workflow.spec.shinyNewKnob": "pending backfill",
			"workflow.spec.gaggle":       "already registered",
		})
		wantViolation(t, violations, `exception for "workflow.spec.gaggle" is obsolete`)
	})
	t.Run("clean", func(t *testing.T) {
		violations := schemaLeafRegistryViolations(t, documents, map[string]string{
			"workflow.spec.shinyNewKnob": "pending backfill",
		})
		if len(violations) != 0 {
			t.Fatalf("violations = %v, want none", violations)
		}
	})
}

// TestSchemaLeafFeatureCandidates pins the selector-to-feature namespace
// conventions shared with internal/authoring's selectorFeatureCandidates
// (which cannot be imported here: internal/authoring imports internal/workflow,
// which wraps this package).
func TestSchemaLeafFeatureCandidates(t *testing.T) {
	tests := []struct {
		leaf string
		want []FeatureID
	}{
		{"workflow.spec.gaggle", []FeatureID{"workflow.spec.gaggle"}},
		{"workflow.spec.tasks.retry.maxAttempts", []FeatureID{"workflow.spec.tasks.retry.maxAttempts", "task.retry.maxAttempts"}},
		{"workflow.spec.tasks.run.command", []FeatureID{"workflow.spec.tasks.run.command", "stage.run.command"}},
		{"workflow.spec.tasks.workspace", []FeatureID{"workflow.spec.tasks.workspace", "stage.workspace"}},
		{"workflow.spec.gates.automated.check", []FeatureID{"workflow.spec.gates.automated.check", "gate.evaluator.automated.check"}},
		{"workflow.spec.gates.branches", []FeatureID{"workflow.spec.gates.branches", "gate.branches"}},
		{"workflow.spec.triggers.selector", []FeatureID{"workflow.spec.triggers.selector", "trigger.selector"}},
		{"goober.spec.role", []FeatureID{"goober.spec.role"}},
	}
	for _, test := range tests {
		got := schemaLeafFeatureCandidates(test.leaf)
		gotStrings := make([]string, len(got))
		for i, id := range got {
			gotStrings[i] = string(id)
		}
		wantStrings := make([]string, len(test.want))
		for i, id := range test.want {
			wantStrings[i] = string(id)
		}
		if strings.Join(gotStrings, ",") != strings.Join(wantStrings, ",") {
			t.Errorf("schemaLeafFeatureCandidates(%q) = %v, want %v", test.leaf, got, test.want)
		}
	}
}

func loadAuthorSchemas(t *testing.T) map[string]map[string]any {
	t.Helper()
	documents := make(map[string]map[string]any, len(authorSchemaFiles))
	for root, file := range authorSchemaFiles {
		raw, err := schemas.FS.ReadFile(file)
		if err != nil {
			t.Fatalf("read embedded schema %s: %v", file, err)
		}
		var document map[string]any
		if err := json.Unmarshal(raw, &document); err != nil {
			t.Fatalf("decode embedded schema %s: %v", file, err)
		}
		documents[root] = document
	}
	return documents
}

// schemaLeafRegistryViolations reports every disagreement between the derived
// schema leaf set, the feature registry, and the exception worklist.
func schemaLeafRegistryViolations(
	t *testing.T,
	documents map[string]map[string]any,
	exceptions map[string]string,
) []string {
	t.Helper()
	leaves := authorSchemaLeaves(t, documents)
	var violations []string
	for _, leaf := range leaves {
		registered := schemaLeafRegistered(leaf)
		reason, excepted := exceptions[leaf]
		switch {
		case registered && excepted:
			violations = append(violations, fmt.Sprintf(
				"exception for %q is obsolete: the leaf resolves to a registered FeatureID — delete the entry", leaf))
		case !registered && !excepted:
			violations = append(violations, fmt.Sprintf(
				"leaf %q has no registered FeatureID (candidates %v) and no exception — register it or add a reasoned exception", leaf, schemaLeafFeatureCandidates(leaf)))
		case excepted && strings.TrimSpace(reason) == "":
			violations = append(violations, fmt.Sprintf(
				"exception for %q has no reason — the worklist must say why the leaf is unregistered", leaf))
		}
	}
	leafSet := make(map[string]bool, len(leaves))
	for _, leaf := range leaves {
		leafSet[leaf] = true
	}
	for _, path := range sortedStringKeys(exceptions) {
		if !leafSet[path] {
			violations = append(violations, fmt.Sprintf(
				"exception for %q names no current schema leaf — delete the stale entry", path))
		}
	}
	return violations
}

// schemaLeafRegistered reports whether a leaf resolves against the current
// registry: an exact FeatureID match, or a namespace whose per-value features
// are registered under the leaf (mirroring internal/authoring's
// featureCandidateKnown prefix rule, e.g. run.network -> stage.run.network.none).
func schemaLeafRegistered(leaf string) bool {
	for _, candidate := range schemaLeafFeatureCandidates(leaf) {
		if _, ok := currentFeatureRegistry.Lookup(candidate); ok {
			return true
		}
		for _, feature := range currentFeatureRegistry.All() {
			if strings.HasPrefix(string(feature.ID), string(candidate)+".") {
				return true
			}
		}
	}
	return false
}

// schemaLeafFeatureCandidates mirrors the namespace conventions of
// internal/authoring's selectorFeatureCandidates: tasks.* maps into the
// task.*/stage.* namespaces, gates.* into gate.*/gate.evaluator.*, and
// triggers.* into trigger.*. Reimplemented here because importing
// internal/authoring from this package would cycle.
func schemaLeafFeatureCandidates(leaf string) []FeatureID {
	candidates := []FeatureID{FeatureID(leaf)}
	switch {
	case strings.HasPrefix(leaf, "workflow.spec.tasks."):
		suffix := strings.TrimPrefix(leaf, "workflow.spec.tasks.")
		switch {
		case suffix == "run" || strings.HasPrefix(suffix, "run."):
			candidates = append(candidates, FeatureID("stage."+suffix))
		case suffix == "workspace":
			candidates = append(candidates, FeatureID("stage.workspace"))
		default:
			candidates = append(candidates, FeatureID("task."+suffix))
		}
	case strings.HasPrefix(leaf, "workflow.spec.gates."):
		suffix := strings.TrimPrefix(leaf, "workflow.spec.gates.")
		switch {
		case suffix == "automated" || strings.HasPrefix(suffix, "automated."),
			suffix == "agentic" || strings.HasPrefix(suffix, "agentic."),
			suffix == "human" || strings.HasPrefix(suffix, "human."):
			candidates = append(candidates, FeatureID("gate.evaluator."+suffix))
		default:
			candidates = append(candidates, FeatureID("gate."+suffix))
		}
	case strings.HasPrefix(leaf, "workflow.spec.triggers."):
		candidates = append(candidates, FeatureID("trigger."+strings.TrimPrefix(leaf, "workflow.spec.triggers.")))
	}
	return candidates
}

// authorSchemaLeaves derives the author-facing leaf paths of each schema:
// dotted instance paths rooted at the document kind, arrays collapsed, local
// $refs resolved at their use site, conditional (if/then/else/not) subschemas
// included. A path is a leaf when no other collected path nests under it.
func authorSchemaLeaves(t *testing.T, documents map[string]map[string]any) []string {
	t.Helper()
	paths := make(map[string]bool)
	for _, root := range sortedStringKeys(documents) {
		collectSchemaPropertyPaths(documents[root], documents[root], root, map[string]bool{}, paths)
	}
	leaves := make([]string, 0, len(paths))
	for path := range paths {
		leaf := true
		for other := range paths {
			if strings.HasPrefix(other, path+".") {
				leaf = false
				break
			}
		}
		if leaf {
			leaves = append(leaves, path)
		}
	}
	sort.Strings(leaves)
	return leaves
}

// collectSchemaPropertyPaths records every property instance path. Annotation
// keywords are skipped so example or default values can never mint paths;
// $defs are only walked where a $ref uses them, matching how
// api/schemas/description_coverage_test.go resolves definitions.
func collectSchemaPropertyPaths(root map[string]any, node any, path string, resolving map[string]bool, paths map[string]bool) {
	switch value := node.(type) {
	case map[string]any:
		if ref, ok := value["$ref"].(string); ok && strings.HasPrefix(ref, "#/") && !resolving[ref] {
			if target, ok := resolveSchemaPointer(root, ref); ok {
				resolving[ref] = true
				collectSchemaPropertyPaths(root, target, path, resolving, paths)
				delete(resolving, ref)
			}
		}
		if properties, ok := value["properties"].(map[string]any); ok {
			for _, name := range sortedStringKeys(properties) {
				childPath := path + "." + name
				paths[childPath] = true
				collectSchemaPropertyPaths(root, properties[name], childPath, resolving, paths)
			}
		}
		for _, key := range sortedStringKeys(value) {
			switch key {
			case "properties", "$defs", "$ref",
				"description", "examples", "default", "enum", "const",
				"pattern", "title", "$schema", "$id", "required":
				continue
			}
			collectSchemaPropertyPaths(root, value[key], path, resolving, paths)
		}
	case []any:
		for _, item := range value {
			collectSchemaPropertyPaths(root, item, path, resolving, paths)
		}
	}
}

func resolveSchemaPointer(root map[string]any, ref string) (any, bool) {
	var current any = root
	for _, component := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		component = strings.ReplaceAll(component, "~1", "/")
		component = strings.ReplaceAll(component, "~0", "~")
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[component]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func sortedStringKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func wantViolation(t *testing.T, violations []string, want string) {
	t.Helper()
	for _, violation := range violations {
		if strings.Contains(violation, want) {
			return
		}
	}
	t.Fatalf("violations %v do not contain %q", violations, want)
}
