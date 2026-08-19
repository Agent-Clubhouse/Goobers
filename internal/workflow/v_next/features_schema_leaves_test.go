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

// schemaLeafExceptions is the authoritative, shrinking worklist for the #3292
// registry backfill: every author-facing schema leaf that has no registered
// FeatureID must appear here with a written reason. Adding a leaf to any of
// the three schemas without either a FeatureID or an entry here fails
// TestEmbeddedSchemaLeavesMapToFeatureRegistry; entries that become registered
// (or leave the schemas) fail it too, so the map can only shrink as #3292
// lands. Reasons beginning with "identity/envelope" mark leaves that are
// deliberately not DSL capabilities and are expected to stay; everything else
// is backfill work.
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

	// Registered under a different FeatureID than the leaf-derived candidates
	// reach — #3292 decides whether to align the id or extend the mapping.
	"workflow.spec.gates.agentic.retry.backoffSeconds":   "registered as gate.evaluator.agentic.retry.backoff (#3292: align id or mapping)",
	"workflow.spec.gates.automated.retry.backoffSeconds": "registered as gate.evaluator.automated.retry.backoff (#3292: align id or mapping)",
	"workflow.spec.gates.human.timeoutSeconds":           "registered as gate.evaluator.human.timeout (#3292: align id or mapping)",
	"workflow.spec.tasks.retry.backoffSeconds":           "registered as task.retry.backoff (#3292: align id or mapping)",
	"workflow.spec.triggers.selector":                    "registered as trigger.backlog-item.selector (#3292: align id or mapping)",
	"workflow.spec.triggers.trustLabel":                  "registered as trigger.backlog-item.trustLabel (#3292: align id or mapping)",

	// Discriminators registered per enum value rather than per field.
	"workflow.spec.tasks.type":    "registered per enum value (task.deterministic, task.agentic); the discriminator has no id of its own",
	"workflow.spec.triggers.type": "registered per enum value (trigger.manual, trigger.backlog-item, ...); the discriminator has no id of its own",

	// Sub-fields of a registered container feature without ids of their own.
	"gaggle.spec.sandbox.agentic":                      "#3292 backfill: sub-field of registered gaggle.spec.sandbox without its own FeatureID",
	"workflow.spec.parallels.branches.name":            "#3292 backfill: sub-field of registered workflow.spec.parallels.branches without its own FeatureID",
	"workflow.spec.parallels.branches.start":           "#3292 backfill: sub-field of registered workflow.spec.parallels.branches without its own FeatureID",
	"workflow.spec.parallels.name":                     "#3292 backfill: sub-field of registered workflow.spec.parallels without its own FeatureID",
	"workflow.spec.runControls.maxRepasses":            "#3292 backfill: sub-field of registered workflow.spec.runControls without its own FeatureID",
	"workflow.spec.runControls.maxRunDuration":         "#3292 backfill: sub-field of registered workflow.spec.runControls without its own FeatureID",
	"workflow.spec.runControls.stalledRunTimeout":      "#3292 backfill: sub-field of registered workflow.spec.runControls without its own FeatureID",
	"goober.spec.mcpServers.args":                      "#3292 backfill: sub-field of registered goober.spec.mcpServers without its own FeatureID",
	"goober.spec.mcpServers.command":                   "#3292 backfill: sub-field of registered goober.spec.mcpServers without its own FeatureID",
	"goober.spec.mcpServers.credentialRefs.capability": "#3292 backfill: sub-field of registered goober.spec.mcpServers without its own FeatureID",
	"goober.spec.mcpServers.credentialRefs.env":        "#3292 backfill: sub-field of registered goober.spec.mcpServers without its own FeatureID",
	"goober.spec.mcpServers.credentialRefs.header":     "#3292 backfill: sub-field of registered goober.spec.mcpServers without its own FeatureID",
	"goober.spec.mcpServers.credentialRefs.kind":       "#3292 backfill: sub-field of registered goober.spec.mcpServers without its own FeatureID",
	"goober.spec.mcpServers.credentialRefs.ref":        "#3292 backfill: sub-field of registered goober.spec.mcpServers without its own FeatureID",
	"goober.spec.mcpServers.credentialRefs.scheme":     "#3292 backfill: sub-field of registered goober.spec.mcpServers without its own FeatureID",
	"goober.spec.mcpServers.name":                      "#3292 backfill: sub-field of registered goober.spec.mcpServers without its own FeatureID",
	"goober.spec.mcpServers.url":                       "#3292 backfill: sub-field of registered goober.spec.mcpServers without its own FeatureID",

	// Trigger fields carried by registered trigger kinds without ids of their own.
	"workflow.spec.triggers.events":              "#3292 backfill: webhook trigger's event list (trigger.webhook is registered) without its own FeatureID",
	"workflow.spec.triggers.idleBackoff.ceiling": "#3292 backfill: schedule-trigger idle-backoff control without a FeatureID",
	"workflow.spec.triggers.idleBackoff.enabled": "#3292 backfill: schedule-trigger idle-backoff control without a FeatureID",
	"workflow.spec.triggers.idleBackoff.floor":   "#3292 backfill: schedule-trigger idle-backoff control without a FeatureID",
	"workflow.spec.triggers.priority":            "#3292 backfill: provider polling priority without a FeatureID",

	// Workflow surface that predates the registry.
	"workflow.spec.docsRoots":                  "#3292 backfill: unregistered DSL surface (docs-updater roots, #472/#1016)",
	"workflow.spec.gates.maxRepasses":          "#3292 backfill: unregistered DSL surface (per-gate re-entry budget)",
	"workflow.spec.outboxMirrorPath":           "#3292 backfill: unregistered DSL surface (journal outbox mirroring)",
	"workflow.spec.requires.capabilities":      "#3292 backfill: unregistered DSL surface (provider-capability requirements, CONF-6/#2079)",
	"workflow.spec.tasks.outbox":               "#3292 backfill: unregistered DSL surface (journal outbox export)",
	"workflow.spec.tasks.outboxMirrorPath":     "#3292 backfill: unregistered DSL surface (journal outbox mirroring)",
	"workflow.spec.tasks.requiredCapabilities": "#3292 backfill: unregistered DSL surface (runner capabilities, RRQ-1/#1101)",
	"workflow.spec.tutorScope.target":          "#3292 backfill: unregistered DSL surface (tutor topology, TUT-A4)",
	"workflow.spec.tutorScope.tier":            "#3292 backfill: unregistered DSL surface (tutor topology, TUT-A4)",

	// Goober persona policy-action surface.
	"goober.spec.conditionalPolicyActions": "#3292 backfill: persona policy-action surface without a FeatureID",
	"goober.spec.policyActions":            "#3292 backfill: persona policy-action surface without a FeatureID",

	// Gaggle surface predates the registry: only gaggle.spec.sandbox and
	// gaggle.spec.project.checkout.sparse are registered today.
	"gaggle.spec.additionalRepos.baseUrl":          "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.additionalRepos.branch":           "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.additionalRepos.checkout.sparse":  "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.additionalRepos.connectionRef":    "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.additionalRepos.name":             "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.additionalRepos.owner":            "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.additionalRepos.project":          "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.additionalRepos.provider":         "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.backlog.baseUrl":                  "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.backlog.connectionRef":            "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.backlog.fieldPredicate":           "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.backlog.labelPredicate":           "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.backlog.labels":                   "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.backlog.project":                  "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.backlog.provider":                 "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.backlog.query":                    "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.branchNamespace":                  "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.ciCommand":                        "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.displayName":                      "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.isolation.identityRef":            "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.isolation.namespace":              "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.outboxMirrorPath":                 "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.project.baseUrl":                  "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.project.branch":                   "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.project.connectionRef":            "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.project.name":                     "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.project.owner":                    "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.project.project":                  "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.project.provider":                 "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.requireLabels":                    "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.requiredCapabilities":             "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.runControls.maxRepasses":          "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.runControls.maxRunDuration":       "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.runControls.stalledRunTimeout":    "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.selfIdentity":                     "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.siblings.label":                   "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.siblings.project.baseUrl":         "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.siblings.project.branch":          "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.siblings.project.checkout.sparse": "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.siblings.project.connectionRef":   "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.siblings.project.name":            "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.siblings.project.owner":           "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.siblings.project.project":         "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.siblings.project.provider":        "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.siblings.requireLabels":           "#3292 backfill: gaggle surface predates the feature registry",
	"gaggle.spec.workcopies.root":                  "#3292 backfill: gaggle surface predates the feature registry",
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
