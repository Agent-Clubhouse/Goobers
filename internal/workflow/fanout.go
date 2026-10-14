package workflow

import (
	"errors"
	"fmt"

	"github.com/goobers/goobers/api/schemas"
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/supportmatrix"
)

// DSL 3.1 conditional and plan-driven fan-out (#1310,
// docs/design/plan-driven-dynamic-fan-out.md). This file owns the router half
// of the authoring contract: the pre-3.1 refusal, the preview feature rows,
// the source-slot rules that need the artifact contract index, the effective
// slot schema lowering (§4.1), and the run-start refusal (§13). The graph
// rules live with the static parallel rules in the v_3_0 interpreter.

// MaxFanOutSourceBytes is the host bound on a fan-out source document: a slot
// declaring a larger maxSize can never be frozen.
const MaxFanOutSourceBytes int64 = 1 << 20

const fanOutSourceMediaType = "application/json"

const (
	featureParallelSelection       FeatureID = "workflow.spec.parallels.selection"
	featureParallelSelectionSource FeatureID = "workflow.spec.parallels.selection.source"
	featureParallelBranchOptional  FeatureID = "workflow.spec.parallels.branches.optional"
	featureMaps                    FeatureID = "workflow.spec.maps"
	featureMapsSource              FeatureID = "workflow.spec.maps.source"
	featureMapsTask                FeatureID = "workflow.spec.maps.task"
	featureMapsBindings            FeatureID = "workflow.spec.maps.bindings"
	featureMapsFailurePolicy       FeatureID = "workflow.spec.maps.failurePolicy"
	featureMapsJoin                FeatureID = "workflow.spec.maps.join"
	featureMapsOnFailure           FeatureID = "workflow.spec.maps.onFailure"
	featureMapsItemTimeoutSeconds  FeatureID = "workflow.spec.maps.itemTimeoutSeconds"
	featureMapsMaxConcurrent       FeatureID = "workflow.spec.maps.maxConcurrent"
)

// ErrSelectiveParallelUnsupported refuses a selective parallel on a runtime
// that cannot freeze its selection. It never runs as a plain parallel.
var ErrSelectiveParallelUnsupported = errors.New("spec.parallels selection (selective parallel) execution is not implemented; remove selection and optional to run this workflow")

// ErrMapUnsupported refuses a map state on a runtime that cannot execute it.
var ErrMapUnsupported = errors.New("spec.maps (data-driven fan-out) execution is not implemented; remove the map to run this workflow")

// UnsupportedFanOutError is the typed run-start refusal for a fan-out
// construct the runtime cannot execute yet. It wraps the matching sentinel so
// callers classify with errors.Is and read State/Feature with errors.As.
type UnsupportedFanOutError struct {
	// State is the selective parallel or map that declares the construct.
	State string
	// Feature is the DSL path, e.g. "spec.maps" or "spec.parallels.selection".
	Feature string
	// Err is ErrSelectiveParallelUnsupported or ErrMapUnsupported.
	Err error
}

func (e *UnsupportedFanOutError) Error() string {
	return fmt.Sprintf("state %q: %v", e.State, e.Err)
}

// Unwrap exposes the sentinel to errors.Is.
func (e *UnsupportedFanOutError) Unwrap() error { return e.Err }

// RefuseUnsupportedFanOut rejects a definition using any DSL 3.1 fan-out
// construct: selective parallels and maps. The engine walk executes neither
// and calls it before any execution effects (design §13).
func RefuseUnsupportedFanOut(spec apiv1.WorkflowSpec) error {
	for _, parallel := range spec.Parallels {
		if parallelIsSelective(parallel) {
			return &UnsupportedFanOutError{State: parallel.Name, Feature: "spec.parallels.selection", Err: ErrSelectiveParallelUnsupported}
		}
	}
	return RefuseUnsupportedLocalFanOut(spec)
}

// RefuseUnsupportedLocalFanOut rejects a definition using a fan-out construct
// the local runner cannot execute yet. Call before any execution effects,
// including when resuming or rerunning a pinned machine. Selective parallels
// run locally since FAN-2 (#7383); maps lift here with FAN-4 (#7385).
func RefuseUnsupportedLocalFanOut(spec apiv1.WorkflowSpec) error {
	if len(spec.Maps) > 0 {
		return &UnsupportedFanOutError{State: spec.Maps[0].Name, Feature: "spec.maps", Err: ErrMapUnsupported}
	}
	return nil
}

func parallelIsSelective(parallel apiv1.Parallel) bool {
	if parallel.Selection != nil {
		return true
	}
	for _, branch := range parallel.Branches {
		if branch.Optional {
			return true
		}
	}
	return false
}

// preV31FanOutSurfaceProblems refuses the fan-out fields on a definition
// pinned below 3.1. The older interpreters never learn them.
func preV31FanOutSurfaceProblems(def Definition, version string) []string {
	var problems []string
	for _, parallel := range def.Spec.Parallels {
		if parallel.Selection != nil {
			problems = append(problems, fmt.Sprintf(
				"parallel %q declares selection, which requires dslVersion %q (this workflow pins %q)",
				parallel.Name, supportmatrix.V31DSLVersion, version))
		}
		for _, branch := range parallel.Branches {
			if branch.Optional {
				problems = append(problems, fmt.Sprintf(
					"parallel %q branch %q declares optional, which requires dslVersion %q (this workflow pins %q)",
					parallel.Name, branch.Name, supportmatrix.V31DSLVersion, version))
			}
		}
	}
	for _, fanOut := range def.Spec.Maps {
		problems = append(problems, fmt.Sprintf(
			"map %q requires dslVersion %q (this workflow pins %q)",
			fanOut.Name, supportmatrix.V31DSLVersion, version))
	}
	return problems
}

func fanOutFeatures() []Feature {
	ids := []FeatureID{
		featureParallelSelection, featureParallelSelectionSource, featureParallelBranchOptional,
		featureMaps, featureMapsSource, featureMapsTask, featureMapsBindings, featureMapsFailurePolicy,
		featureMapsJoin, featureMapsOnFailure, featureMapsItemTimeoutSeconds, featureMapsMaxConcurrent,
	}
	features := make([]Feature, 0, len(ids))
	for _, id := range ids {
		features = append(features, Feature{
			ID: id, Level: SupportPreview, SinceVersion: "dev",
			History:     []SupportTransition{{Level: SupportPreview, SinceVersion: "dev"}},
			DSLVersions: []DSLFeatureSupport{{Version: supportmatrix.V31DSLVersion, Level: SupportPreview}},
		})
	}
	return features
}

func usedFanOutFeatures(def Definition) []Feature {
	used := map[FeatureID]bool{}
	for _, parallel := range def.Spec.Parallels {
		if parallel.Selection != nil {
			used[featureParallelSelection], used[featureParallelSelectionSource] = true, true
		}
		for _, branch := range parallel.Branches {
			used[featureParallelBranchOptional] = used[featureParallelBranchOptional] || branch.Optional
		}
	}
	for _, fanOut := range def.Spec.Maps {
		for _, id := range []FeatureID{featureMaps, featureMapsSource, featureMapsTask, featureMapsFailurePolicy, featureMapsJoin} {
			used[id] = true
		}
		used[featureMapsBindings] = used[featureMapsBindings] || fanOut.Bindings != nil
		used[featureMapsOnFailure] = used[featureMapsOnFailure] || fanOut.OnFailure != ""
		used[featureMapsItemTimeoutSeconds] = used[featureMapsItemTimeoutSeconds] || fanOut.ItemTimeoutSeconds != 0
		used[featureMapsMaxConcurrent] = used[featureMapsMaxConcurrent] || fanOut.MaxConcurrent != 0
	}
	var features []Feature
	for _, feature := range fanOutFeatures() {
		if used[feature.ID] {
			features = append(features, feature)
		}
	}
	return features
}

// fanOutSource is one declared source with the state that reads it.
type fanOutSource struct {
	label    string // "parallel \"review\" selection" or "map \"run-shards\""
	state    string
	source   apiv1.FanOutSource
	envelope string
}

func fanOutSources(def Definition) []fanOutSource {
	var sources []fanOutSource
	for _, parallel := range def.Spec.Parallels {
		if parallel.Selection != nil {
			sources = append(sources, fanOutSource{
				label: fmt.Sprintf("parallel %q selection", parallel.Name), state: parallel.Name,
				source: parallel.Selection.Source, envelope: schemas.FanOutSelection,
			})
		}
	}
	for _, fanOut := range def.Spec.Maps {
		sources = append(sources, fanOutSource{
			label: fmt.Sprintf("map %q", fanOut.Name), state: fanOut.Name,
			source: fanOut.Source, envelope: schemas.FanOutRoster,
		})
	}
	return sources
}

// lowerFanOutSources checks every fan-out source against the artifact
// contract index and the compiled graph, and lowers the matching host
// envelope schema onto each source slot that declares no schemaPath of its
// own (design §4.1). That effective schema is what the producer's
// publish_output validation (#7060) enforces. A slot with its own schemaPath
// keeps it; the runner checks the envelope again when it freezes the document.
func lowerFanOutSources(machine *Machine, index artifactContractIndex) (map[string]map[string]string, []CompileDiagnostic) {
	var diagnostics []CompileDiagnostic
	envelopes := map[string]map[string]string{}
	claimed := map[string]fanOutSource{}
	for _, source := range fanOutSources(machine.Def) {
		slot, problems := fanOutSourceProblems(machine, index, source)
		diagnostics = append(diagnostics, problems...)
		if len(problems) > 0 {
			continue
		}
		key := source.source.Task + "." + source.source.Slot
		if prior, ok := claimed[key]; ok && prior.envelope != source.envelope {
			diagnostics = append(diagnostics, slotDiagnostic(source.source.Task, source.source.Slot,
				"%s source %s is also the source of %s; one slot cannot hold both a selection and a roster",
				source.label, key, prior.label))
			continue
		}
		claimed[key] = source
		if slot.SchemaPath != "" {
			continue
		}
		if envelopes[source.source.Task] == nil {
			envelopes[source.source.Task] = map[string]string{}
		}
		envelopes[source.source.Task][source.source.Slot] = source.envelope
	}
	sortDiagnostics(diagnostics)
	if len(envelopes) == 0 {
		envelopes = nil
	}
	return envelopes, diagnostics
}

func fanOutSourceProblems(machine *Machine, index artifactContractIndex, source fanOutSource) (apiv1.ArtifactSlot, []CompileDiagnostic) {
	task, slotName := source.source.Task, source.source.Slot
	fail := func(format string, args ...any) (apiv1.ArtifactSlot, []CompileDiagnostic) {
		return apiv1.ArtifactSlot{}, []CompileDiagnostic{slotDiagnostic(task, slotName,
			"%s source %s", source.label, fmt.Sprintf(format, args...))}
	}
	if _, ok := machine.Task(task); !ok {
		if kind := nonTaskStateKind(machine, task); kind != "" {
			return fail("task %q is a %s; a fan-out source must be a task", task, kind)
		}
		return fail("task %q is not a defined task", task)
	}
	slot, ok := index.slots[task][slotName]
	if !ok {
		return fail("slot %q is not an artifactSlots entry of task %q", slotName, task)
	}
	if slot.MediaType != fanOutSourceMediaType {
		return fail("slot %q of task %q must declare mediaType %q", slotName, task, fanOutSourceMediaType)
	}
	if slot.MaxSize > MaxFanOutSourceBytes {
		return fail("slot %q of task %q declares maxSize %d, above the host bound of %d bytes", slotName, task, slot.MaxSize, MaxFanOutSourceBytes)
	}
	if !artifactProducerDominatesConsumer(machine, task, source.state) {
		return fail("task %q does not run on every path before %q", task, source.state)
	}
	return slot, nil
}

func nonTaskStateKind(machine *Machine, name string) string {
	if _, ok := machine.Gate(name); ok {
		return "gate"
	}
	if _, ok := machine.Parallel(name); ok {
		return "parallel"
	}
	if _, ok := machine.Map(name); ok {
		return "map"
	}
	return ""
}
