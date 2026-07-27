// Package failfirst enforces TUT-A2's fail-first validation-authorship
// contract (#1214, docs/design/tutor-redesign.md §5.1): every tutor-authored
// workflow-level test/validation stage must be demonstrated red against the
// pre-change config (it reproduces the process regression) before green
// against the fix — a vacuously-passing check that "closes" a finding games
// the loop. A workflow's Gates ARE its "validation/branching states" (see the
// doc comment on apiv1.WorkflowSpec.Gates), so a new Gate on a changed
// workflows/*.yaml file is this package's structural signal that a run added
// a validation stage.
//
// This package is pure diff/evidence logic, mirroring the
// internal/configboundary split: the git plumbing (listing changed files,
// reading a path's content at the base ref) lives in the cmd/goobers stage
// command, not here.
package failfirst

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// ErrMissingEvidence marks a newly added gate with no evidence entry at all.
var ErrMissingEvidence = errors.New("failfirst: no fail-first evidence for gate")

// ErrNotFailFirst marks a gate whose evidence entry does not assert the
// required red-then-green result (or omits its provenance citation).
var ErrNotFailFirst = errors.New("failfirst: evidence does not show a fail-first (red-then-green) result")

// Required verdict strings a GateEvidence entry must assert, verbatim.
const (
	VerdictFail = "fail"
	VerdictPass = "pass"
)

// GateRef identifies one Gate added by a changed workflow file.
type GateRef struct {
	File     string // repo-relative path of the workflows/*.yaml file
	Workflow string // the Workflow's metadata.name, if resolvable
	Gate     string // the added Gate's name
}

// Key is this gate's lookup key in an Evidence's Gates map: "<file>#<gate>".
// Keying by file (not gate name alone) means two different workflows adding a
// same-named gate each need their own evidence entry.
func (g GateRef) Key() string {
	return g.File + "#" + g.Gate
}

// IsWorkflowFile reports whether p names a workflow definition under a
// `workflows/` directory — the `**/workflows/*.yaml` action class (design doc
// §4.2) whose Gates this package treats as workflow-level validation stages.
func IsWorkflowFile(p string) bool {
	clean := path.Clean(strings.ReplaceAll(p, `\`, "/"))
	if !strings.HasSuffix(clean, ".yaml") && !strings.HasSuffix(clean, ".yml") {
		return false
	}
	dir := path.Dir(clean)
	return dir == "workflows" || strings.HasSuffix(dir, "/workflows")
}

// GateNames parses content as a Workflow and returns its declared gate names.
// Empty content — a file that does not exist on one side of the diff — is not
// an error; it simply declares no gates.
func GateNames(content []byte) (map[string]bool, error) {
	names := map[string]bool{}
	if strings.TrimSpace(string(content)) == "" {
		return names, nil
	}
	var w apiv1.Workflow
	if err := yaml.Unmarshal(content, &w); err != nil {
		return nil, fmt.Errorf("parse workflow: %w", err)
	}
	for _, g := range w.Spec.Gates {
		names[g.Name] = true
	}
	return names, nil
}

// NewGates reports every gate present in newContent's workflow but absent
// from oldContent's — i.e. every gate this branch adds to file, sorted by
// name for deterministic output.
//
// oldContent that fails to parse (e.g. the file did not exist before this
// branch, or was never a valid workflow) is treated as declaring no gates
// rather than erroring, so every gate in newContent counts as added. This is
// deliberately the fail-closed direction: over-detecting a "new" gate asks
// for more fail-first evidence, never less.
func NewGates(file string, oldContent, newContent []byte) ([]GateRef, error) {
	oldNames, err := GateNames(oldContent)
	if err != nil {
		oldNames = map[string]bool{}
	}
	newNames, err := GateNames(newContent)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	var workflow string
	if strings.TrimSpace(string(newContent)) != "" {
		var w apiv1.Workflow
		if err := yaml.Unmarshal(newContent, &w); err == nil {
			workflow = w.Name
		}
	}
	var added []GateRef
	for name := range newNames {
		if !oldNames[name] {
			added = append(added, GateRef{File: file, Workflow: workflow, Gate: name})
		}
	}
	sort.Slice(added, func(i, j int) bool { return added[i].Gate < added[j].Gate })
	return added, nil
}

// GateRefNames returns each ref's Key(), in the order given — used for
// stable, human-readable stdout/error text.
func GateRefNames(refs []GateRef) []string {
	names := make([]string, len(refs))
	for i, r := range refs {
		names[i] = r.Key()
	}
	return names
}

// Evidence is the JSON artifact a tutor-authored validation-stage change
// commits alongside itself to satisfy the fail-first contract: for every new
// gate, proof it reproduced the regression (red) against the pre-change
// config before passing (green) against the fix.
type Evidence struct {
	Gates map[string]GateEvidence `json:"gates"`
}

// GateEvidence names the two required verdicts, verbatim, plus a provenance
// citation. This package does not itself re-execute the gate against both
// configs — a workflow-level gate can cross-cut arbitrary process state this
// stage cannot generically re-run — it mechanically requires the change to
// assert and cite the red-then-green result, closing the "silently omit
// fail-first" path that prose guidance alone cannot: the gate this evidence
// feeds fails the run closed when the assertion is absent or wrong, exactly
// as config-valid fails closed on an invalid draft.
type GateEvidence struct {
	PreFix  string `json:"preFix"`
	PostFix string `json:"postFix"`
	// RunEvidence cites the run(s)/journal pointer(s) demonstrating the
	// red-then-green result (governance §5.6 provenance discipline).
	RunEvidence string `json:"runEvidence,omitempty"`
}

// VerifyEvidence returns nil only when every gate in newGates has an Evidence
// entry (keyed by GateRef.Key()) asserting PreFix==VerdictFail,
// PostFix==VerdictPass, and a non-empty RunEvidence citation.
func VerifyEvidence(newGates []GateRef, evidence Evidence) error {
	for _, g := range newGates {
		e, ok := evidence.Gates[g.Key()]
		if !ok {
			return fmt.Errorf("%w: %s (workflow %q, gate %q) — evidence must include a \"gates\" entry keyed %q",
				ErrMissingEvidence, g.Key(), g.Workflow, g.Gate, g.Key())
		}
		if e.PreFix != VerdictFail || e.PostFix != VerdictPass {
			return fmt.Errorf("%w: %s must assert preFix=%q and postFix=%q, got preFix=%q postFix=%q",
				ErrNotFailFirst, g.Key(), VerdictFail, VerdictPass, e.PreFix, e.PostFix)
		}
		if strings.TrimSpace(e.RunEvidence) == "" {
			return fmt.Errorf("%w: %s: runEvidence citation (run-id/journal pointer) required",
				ErrNotFailFirst, g.Key())
		}
	}
	return nil
}
