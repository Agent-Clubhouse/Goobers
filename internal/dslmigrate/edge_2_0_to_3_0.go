package dslmigrate

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
	k8syaml "sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	v30 "github.com/goobers/goobers/internal/workflow/v_3_0"
)

// applyNextToV3 is the DSL 2.0→3.0 migration edge (dsl-3.0.md §6, decision
// record D12/D13/D16, Goobernetes-Delivery decisions 001/002). It rewrites a
// 2.0 workflow (or gaggle) document into its 3.0 form, per-rule and
// deterministic:
//
//  2. task and gaggle `requiredCapabilities` → `runsOn.capabilities` (grammar
//     unchanged);
//  3. `os=<goos>` tokens are lifted out of the capability set and become
//     `runsOn.os` (os=linux→linux, os=windows→windows, os=darwin→macOS) — the
//     canonical spelling is the product name, not GOOS. Two DIFFERENT os
//     tokens on one stage, or an os token naming a platform 3.0 has no enum
//     for, is a REFUSAL (it was already unsatisfiable, or would land as a
//     CAP004 error after migration);
//  4. `run.network: none` → `runsOn.restrictions: [network:none]` (D16);
//  5. `repoFrom` edges COMPUTED AS REACHING DEFINITIONS over the stage graph
//     and inserted automatically — the analysis is the interpreter's own
//     v30.RepoFromCoverage (delivery decisions 001/002: gate-fail routing
//     included, fixed-point over cycles, a consumer's own prior attempts
//     excluded, producers per the commit reading). Reusing the compiler's
//     analysis is deliberate: a second, differently-written analysis here
//     would let the migrator and the WF022 compile check disagree. Refusal
//     only where coverage cannot be computed (a structurally broken graph);
//  6. the preview-surface rewrites: external call-out's GA spelling (expected
//     identical to the 2.0 preview spelling — no-op today) and a refusal for
//     the per-gaggle sandbox override, which folds into the restrictions model
//     but has no migrator equivalent yet (#3516).
//
// Rule 1 (the dslVersion pin itself) is applied by Migrate around this edge.
func applyNextToV3(source []byte, root *yaml.Node) (bool, []string, error) {
	kind := ""
	if kindNode, _ := mapValue(root, "kind"); kindNode != nil {
		kind = kindNode.Value
	}
	spec, _ := mapValue(root, "spec")

	// Rule 6: refuse the per-gaggle sandbox override up front — it has no 3.0
	// equivalent yet (folds into the restrictions model, #3516) and silently
	// dropping it would weaken an isolation posture the author declared.
	if spec != nil {
		if sandbox, _ := mapValue(spec, "sandbox"); sandbox != nil {
			return false, nil, fmt.Errorf(
				"spec.sandbox (the per-gaggle sandbox override) has no DSL 3.0 equivalent yet: it folds into the runsOn.restrictions model but the fold is not implemented (#3516) — remove it or wait for the restrictions migrator before migrating this document")
		}
	}

	var notes []string
	changed := false

	if kind == "Gaggle" {
		gaggleChanged, err := migrateGaggleSpec(spec, &notes)
		if err != nil {
			return false, nil, err
		}
		changed = changed || gaggleChanged
		return changed, notes, nil
	}

	// Workflow path.
	if spec == nil {
		return false, nil, nil
	}
	tasks, _ := mapValue(spec, "tasks")

	// Rules 2-4, per task.
	if tasks != nil {
		for _, task := range tasks.Content {
			taskName := ""
			if nameNode, _ := mapValue(task, "name"); nameNode != nil {
				taskName = nameNode.Value
			}
			taskChanged, err := migrateStageRunsOn(task, taskName, &notes)
			if err != nil {
				return false, nil, err
			}
			changed = changed || taskChanged
		}
	}

	// Rule 5: declared repo-handoff edges, computed as reaching definitions by
	// the interpreter's own analysis.
	repoChanged, err := insertRepoFromEdges(source, tasks, &notes)
	if err != nil {
		return false, nil, err
	}
	changed = changed || repoChanged

	return changed, notes, nil
}

// migrateGaggleSpec migrates a gaggle document's spec.requiredCapabilities to a
// gaggle-level spec.runsOn carrying os/capabilities only (dsl-3.0.md §2 "Gaggle
// level"). NOTE: `goobers fix` today iterates only workflow files, not gaggle
// files, so this arm is reached only when a gaggle document is passed
// directly; wiring the fix command to sweep gaggle documents is the follow-up
// recorded in the PR.
func migrateGaggleSpec(spec *yaml.Node, notes *[]string) (bool, error) {
	if spec == nil {
		return false, nil
	}
	rc, _ := mapValue(spec, "requiredCapabilities")
	if rc == nil {
		return false, nil
	}
	osValue, caps, err := splitOSTokens(sequenceValues(rc), "gaggle")
	if err != nil {
		return false, err
	}
	removeMapKey(spec, "requiredCapabilities")
	runsOn := ensureChildMap(spec, "runsOn")
	if osValue != "" {
		setScalar(runsOn, "os", osValue, "!!str")
	}
	if len(caps) > 0 {
		setFlowSequence(runsOn, "capabilities", caps)
	}
	*notes = append(*notes, "gaggle: migrated requiredCapabilities to runsOn (capabilities + os)")
	return true, nil
}

// migrateStageRunsOn applies rules 2-4 to one task node.
func migrateStageRunsOn(task *yaml.Node, taskName string, notes *[]string) (bool, error) {
	changed := false

	if rc, _ := mapValue(task, "requiredCapabilities"); rc != nil {
		osValue, caps, err := splitOSTokens(sequenceValues(rc), fmt.Sprintf("task %q", taskName))
		if err != nil {
			return false, err
		}
		removeMapKey(task, "requiredCapabilities")
		runsOn := ensureChildMap(task, "runsOn")
		if osValue != "" {
			setScalar(runsOn, "os", osValue, "!!str")
		}
		if len(caps) > 0 {
			setFlowSequence(runsOn, "capabilities", caps)
		}
		*notes = append(*notes, fmt.Sprintf(
			"task %q: migrated requiredCapabilities to runsOn.capabilities%s", taskName, osNote(osValue)))
		changed = true
	}

	// Rule 4: run.network: none → runsOn.restrictions: [network:none].
	if run, _ := mapValue(task, "run"); run != nil {
		if net, _ := mapValue(run, "network"); net != nil && net.Value == string(apiv1.NetworkNone) {
			removeMapKey(run, "network")
			runsOn := ensureChildMap(task, "runsOn")
			appendToFlowSequence(runsOn, "restrictions", "network:none")
			*notes = append(*notes, fmt.Sprintf(
				"task %q: migrated run.network: none to runsOn.restrictions: [network:none]", taskName))
			changed = true
		}
	}

	return changed, nil
}

// insertRepoFromEdges is rule 5: it parses the original source into the typed
// spec, computes the reaching-definitions coverage with the interpreter's own
// v30.RepoFromCoverage, and inserts a repoFrom declaration on every
// repo-consuming stage whose coverage is non-empty — a scalar for a single
// producer, a flow list for CI-repass fan-in. It refuses only when coverage
// cannot be computed (a structurally broken graph the compiler reports
// elsewhere).
func insertRepoFromEdges(source []byte, tasks *yaml.Node, notes *[]string) (bool, error) {
	if tasks == nil {
		return false, nil
	}
	var wf apiv1.Workflow
	if err := k8syaml.Unmarshal(source, &wf); err != nil {
		return false, fmt.Errorf("parse workflow to compute repoFrom edges: %w", err)
	}
	def := v30.Definition{Name: wf.Name, Version: 1, DSLVersion: v30.DSLVersion, Spec: wf.Spec}
	coverage := v30.RepoFromCoverage(def)
	if coverage == nil {
		return false, fmt.Errorf(
			"cannot compute repoFrom handoff edges: the stage graph is structurally invalid — fix the reported graph errors, then migrate (dsl-3.0.md §6 rule 5)")
	}

	changed := false
	for _, task := range tasks.Content {
		nameNode, _ := mapValue(task, "name")
		if nameNode == nil {
			continue
		}
		producers, ok := coverage[nameNode.Value]
		if !ok || len(producers) == 0 {
			continue
		}
		// Deterministic order: the coverage is already sorted, keep it.
		sorted := append([]string(nil), producers...)
		sort.Strings(sorted)
		if len(sorted) == 1 {
			setScalar(task, "repoFrom", sorted[0], "!!str")
		} else {
			setFlowSequence(task, "repoFrom", sorted)
		}
		*notes = append(*notes, fmt.Sprintf(
			"task %q: declared repoFrom %s (reaching-definitions handoff edge, WF022)", nameNode.Value, repoFromRender(sorted)))
		changed = true
	}
	return changed, nil
}

// splitOSTokens partitions a requiredCapabilities set into an os enum value and
// the remaining (non-os) capability tokens. It refuses two DIFFERENT os tokens
// (already unsatisfiable) or an os token naming a platform DSL 3.0 has no enum
// for (it would land as CAP004 after migration). where names the stage for the
// diagnostic.
func splitOSTokens(tokens []string, where string) (osValue string, caps []string, err error) {
	seen := map[string]bool{}
	for _, tok := range tokens {
		if !strings.HasPrefix(tok, "os=") {
			caps = append(caps, tok)
			continue
		}
		goos := strings.TrimPrefix(tok, "os=")
		mapped, ok := osEnum[goos]
		if !ok {
			return "", nil, fmt.Errorf(
				"%s declares capability %q, which DSL 3.0 cannot express: runsOn.os is the enum linux|windows|macOS and has no value for GOOS %q — remove it or target a supported platform before migrating (dsl-3.0.md §6 rule 3)",
				where, tok, goos)
		}
		seen[mapped] = true
	}
	if len(seen) > 1 {
		var got []string
		for v := range seen {
			got = append(got, v)
		}
		sort.Strings(got)
		return "", nil, fmt.Errorf(
			"%s declares conflicting os tokens resolving to %s — a stage runs on exactly one OS, so this was already unsatisfiable; declare a single os before migrating (dsl-3.0.md §6 rule 3)",
			where, strings.Join(got, ", "))
	}
	for v := range seen {
		osValue = v
	}
	return osValue, caps, nil
}

// osEnum maps the 2.0 os=<goos> token payloads to the 3.0 runsOn.os enum. The
// canonical spelling is the product name (macOS), not GOOS (darwin).
var osEnum = map[string]string{
	"linux":   "linux",
	"windows": "windows",
	"darwin":  "macOS",
}

func osNote(osValue string) string {
	if osValue == "" {
		return ""
	}
	return fmt.Sprintf(" and lifted the os token to runsOn.os: %s", osValue)
}

func repoFromRender(names []string) string {
	if len(names) == 1 {
		return names[0]
	}
	return "[" + strings.Join(names, ", ") + "]"
}

// --- yaml.Node helpers ---

// sequenceValues returns the scalar string values of a sequence node (block or
// flow); a non-sequence returns nil.
func sequenceValues(n *yaml.Node) []string {
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil
	}
	values := make([]string, 0, len(n.Content))
	for _, item := range n.Content {
		if item.Kind == yaml.ScalarNode {
			values = append(values, item.Value)
		}
	}
	return values
}

// removeMapKey deletes key (and its value) from a mapping node.
func removeMapKey(n *yaml.Node, key string) {
	if n == nil || n.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			n.Content = append(n.Content[:i], n.Content[i+2:]...)
			return
		}
	}
}

// ensureChildMap returns the mapping node stored at key, creating an empty one
// (appended to n) when absent.
func ensureChildMap(n *yaml.Node, key string) *yaml.Node {
	if v, _ := mapValue(n, key); v != nil {
		if v.Kind == yaml.MappingNode {
			return v
		}
	}
	child := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	n.Content = append(n.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key, Tag: "!!str"},
		child,
	)
	return child
}

// setFlowSequence sets key to a flow-style sequence of string scalars, adding
// the key if absent.
func setFlowSequence(n *yaml.Node, key string, values []string) {
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
	for _, v := range values {
		seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: v, Tag: "!!str"})
	}
	if existing, idx := mapValue(n, key); existing != nil {
		n.Content[idx] = seq
		return
	}
	n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key, Tag: "!!str"}, seq)
}

// appendToFlowSequence appends value to the flow sequence at key, creating the
// sequence when absent and de-duplicating.
func appendToFlowSequence(n *yaml.Node, key, value string) {
	existing, _ := mapValue(n, key)
	if existing == nil || existing.Kind != yaml.SequenceNode {
		setFlowSequence(n, key, []string{value})
		return
	}
	for _, item := range existing.Content {
		if item.Value == value {
			return
		}
	}
	existing.Content = append(existing.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: value, Tag: "!!str"})
}
