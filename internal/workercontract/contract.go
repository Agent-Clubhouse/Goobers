// Package workercontract describes this binary's interpreter contracts. It
// provides offline evidence only: it neither admits work nor changes routing.
package workercontract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/goobers/goobers/internal/supportmatrix"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/workflow/ir"
)

// SchemaVersion identifies the evidence format, independently of runtime input.
const SchemaVersion = "goobers.dev/worker-contract/v1"

// Manifest is this executable's declared support, not an attestation about a
// remote worker. BuildID is an equality identity, never a compatibility range.
type Manifest struct {
	SchemaVersion    string        `json:"schemaVersion"`
	BuildID          string        `json:"buildId"`
	ExecutableDigest string        `json:"executableDigest"`
	IRSchema         string        `json:"irSchema"`
	Compiler         ir.Compiler   `json:"compiler"`
	DSL              []DSLContract `json:"dsl"`
}

// DSLContract retains preview/deprecation posture rather than flattening every
// registered interpreter into a promise that every workflow is executable.
type DSLContract struct {
	Version  string    `json:"version"`
	Level    string    `json:"level"`
	Features []Feature `json:"features"`
}

// Feature is one explicit interpreter feature and its support level.
type Feature struct {
	ID    string `json:"id"`
	Level string `json:"level"`
}

// Current derives support from the same registries used by compilation.
func Current(buildID, executableDigest string) (Manifest, error) {
	m := Manifest{SchemaVersion: SchemaVersion, BuildID: buildID, ExecutableDigest: executableDigest,
		IRSchema: ir.SchemaVersion, Compiler: ir.Compiler{Name: ir.CompilerName, Version: ir.CompilerVersion}}
	for _, version := range supportmatrix.GetDSL().Versions() {
		if version.Level == supportmatrix.LevelUnsupported {
			continue
		}
		features, err := workflow.FeaturesAtDSLVersion(workflow.AllFeatures(), version.Version)
		if err != nil {
			return Manifest{}, err
		}
		dsl := DSLContract{Version: version.Version, Level: string(version.Level), Features: []Feature{}}
		for _, f := range features {
			dsl.Features = append(dsl.Features, Feature{ID: string(f.ID), Level: string(f.Level)})
		}
		m.DSL = append(m.DSL, dsl)
	}
	return m, nil
}

// Digest is a content identity for an emitted manifest, not a signature.
func (m Manifest) Digest() (string, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Inspection reports what this binary can prove about an IR's exact persisted
// source. Compatible is intentionally separate from admission and readiness.
type Inspection struct {
	State        string    `json:"state"`
	Enforced     bool      `json:"enforced"`
	SourceDigest string    `json:"sourceDigest,omitempty"`
	IRDigest     string    `json:"irDigest,omitempty"`
	DSLVersion   string    `json:"dslVersion,omitempty"`
	AllowPreview bool      `json:"allowPreview"`
	Requirements []Feature `json:"requirements,omitempty"`
	Reason       string    `json:"reason"`
}

// Inspect refuses to infer requirements from source-less IR or cosmetic
// FeatureGates. The persisted source must match both its digest and the IR's
// normalized semantics, then pass the same preflight guards and compiler as Run.
func Inspect(doc ir.Document, allowPreview bool) Inspection {
	i := Inspection{State: "unverifiable", AllowPreview: allowPreview}
	if doc.SourceDefinition == nil {
		i.Reason = "exact source definition is absent; source digest and runtime requirements cannot be verified"
		return i
	}
	if err := ir.Validate(doc); err != nil {
		i.Reason = fmt.Sprintf("invalid workflow IR: %v", err)
		return i
	}
	if doc.Compiler.Name != ir.CompilerName || doc.Compiler.Version != ir.CompilerVersion {
		i.Reason = "compiler contract is not supported by this binary"
		return i
	}
	normalized, err := ir.NormalizeWithMetadata(*doc.SourceDefinition, doc.Provenance, doc.FeatureGates)
	if err != nil {
		i.Reason = fmt.Sprintf("cannot normalize exact source: %v", err)
		return i
	}
	// Canonical bytes compare every graph field, including annotations whose
	// omission from a semantic-diff helper would weaken this proof.
	want, err := normalized.Digest()
	if err != nil {
		i.Reason = err.Error()
		return i
	}
	got, err := doc.Digest()
	if err != nil || got != want {
		i.Reason = "IR does not match the canonical normalization of its exact source"
		return i
	}
	i.SourceDigest, i.IRDigest, i.DSLVersion = doc.Source.Digest, got, doc.Source.DSLVersion
	i.State = "incompatible"
	def := *doc.SourceDefinition
	if err := workflow.RefuseChildWorkflowExecution(def.Spec); err != nil {
		i.Reason = err.Error()
		return i
	}
	if err := workflow.RefuseUnsupportedFanOut(def.Spec); err != nil {
		i.Reason = err.Error()
		return i
	}
	for _, diagnostic := range workflow.CheckWorkflowFeatureSupport(def, allowPreview) {
		if diagnostic.Blocking {
			i.Reason = diagnostic.Message
			return i
		}
	}
	if _, err := workflow.Compile(def, workflow.WithPreviewFeatures(allowPreview)); err != nil {
		i.Reason = err.Error()
		return i
	}
	features, err := workflow.FeaturesForWorkflow(def)
	if err != nil {
		i.Reason = err.Error()
		return i
	}
	for _, feature := range features {
		i.Requirements = append(i.Requirements, Feature{ID: string(feature.ID), Level: string(feature.Level)})
	}
	slices.SortFunc(i.Requirements, func(a, b Feature) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	i.State = "compatible"
	i.Reason = "exact source compiles with this binary; routing, remote workers, credentials, placements and resources are not verified"
	return i
}
