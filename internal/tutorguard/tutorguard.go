// Package tutorguard implements TUT-A3, the Tutor's metric-gaming guard
// (design: docs/design/tutor-redesign.md §5 item 2, issue #1215). The Tutor
// mines gate-noise findings (gate-never-fails / gate-repass-churn) and may
// propose editing the very gate that produced the finding; the cheapest way
// to "improve" that metric is to loosen or delete the noisy gate, which can
// strip real coverage rather than fix a real problem (a noisy gate may be
// miscalibrated, not dead — see shared-gate-evaluator-blast-radius, #415).
//
// This package holds the two pieces of logic the guard needs: parsing the
// analyst's finding.md for the machine-readable fields config-author and the
// guard both key off (Kind/Subject/independent-proof), and classifying what a
// drafted change actually did to the named gate's definition, before/after.
// The guard never inspects intent — only the observable diff.
package tutorguard

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// ErrNoFrontMatter means finding.md has no `---`-delimited front matter block
// at all — the analyst wrote pure prose. Callers treat this as "nothing to
// enforce": a pre-TUT-A3 finding.md, or one about a change that isn't gate
// noise, carries no Kind/Subject to check a diff against.
var ErrNoFrontMatter = errors.New("tutorguard: finding.md has no front matter block")

// FindingMeta is the machine-readable header the analyst goober's finding.md
// must carry (reference-workflows/gaggles/goobers/goobers/analyst/instructions.md) so a
// deterministic stage — not just a human reviewer — can check what kind of
// finding prompted this run's proposed change, which gate (if any) it names,
// and whether the analyst recorded independent proof that gate is dead.
type FindingMeta struct {
	// Kind mirrors internal/telemetry/rollup.FindingKind, e.g.
	// "gate-never-fails" or "gate-repass-churn". Empty for a finding kind the
	// guard has nothing to say about (only the two gate-noise kinds name a
	// specific gate whose removal/loosening would be self-serving).
	Kind string `json:"kind"`
	// Subject is the exact gate name the finding's Metrics/Threshold were
	// computed for (rollup.Finding.Subject) — the one gate this run's diff
	// must not remove or loosen without IndependentProof.
	Subject string `json:"subject"`
	// IndependentProof is the analyst's cited evidence — distinct from the
	// noise metric itself — that Subject is actually dead weight rather than
	// miscalibrated (e.g. a manual audit, or evidence the underlying check is
	// permanently unreachable). Empty means no proof was offered.
	IndependentProof string `json:"independentProof"`
}

// IsGateNoise reports whether m.Kind is one of the two gate-noise finding
// kinds the guard cares about (rollup.FindingGateNeverFails /
// rollup.FindingGateRepassChurn). Named as strings here, not by importing
// internal/telemetry/rollup, to keep this package's front-matter parsing
// independent of the rollup schema — the guard only needs the two exact
// string values, which are a stable, documented finding-kind vocabulary.
func (m FindingMeta) IsGateNoise() bool {
	return m.Kind == "gate-never-fails" || m.Kind == "gate-repass-churn"
}

// HasIndependentProof reports whether the analyst recorded non-trivial
// evidence — not just whitespace — that Subject is dead.
func (m FindingMeta) HasIndependentProof() bool {
	return strings.TrimSpace(m.IndependentProof) != ""
}

// ParseFindingMarkdown extracts FindingMeta from finding.md's front matter: a
// `---`-delimited YAML block at the top of the file, mirroring the front
// matter every goober instructions.md in this repo already uses. A
// finding.md with no front matter (ErrNoFrontMatter) or a front-matter block
// that fails to parse as YAML both return a zero FindingMeta — the guard's
// caller treats "can't tell what this finding was about" the same as "not
// gate noise", never as a reason to block a legitimate, unrelated change.
func ParseFindingMarkdown(data []byte) (FindingMeta, error) {
	block, ok := frontMatterBlock(data)
	if !ok {
		return FindingMeta{}, ErrNoFrontMatter
	}
	var meta FindingMeta
	if err := yaml.Unmarshal(block, &meta); err != nil {
		return FindingMeta{}, fmt.Errorf("tutorguard: parse finding.md front matter: %w", err)
	}
	return meta, nil
}

// frontMatterBlock returns the bytes between the first two `---` delimiter
// lines, if the document opens with one.
func frontMatterBlock(data []byte) ([]byte, bool) {
	lines := bytes.Split(data, []byte("\n"))
	if len(lines) == 0 || strings.TrimSpace(string(lines[0])) != "---" {
		return nil, false
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(string(lines[i])) == "---" {
			return bytes.Join(lines[1:i], []byte("\n")), true
		}
	}
	return nil, false
}

// GateEditKind classifies what a run's diff did to the specific gate a
// finding flagged.
type GateEditKind string

const (
	// GateEditNone means the named gate is byte-for-byte unchanged (or was
	// never present in either revision) — the run's diff didn't touch it.
	GateEditNone GateEditKind = "none"
	// GateEditTuning means the gate still exists and still fails closed
	// (its fail branch still routes to the same blocking target it did
	// before) — some other field changed, e.g. its check parameters or
	// timeout. This is the allowed, unrestricted "tighten it" path
	// TUT-A3's acceptance criteria calls gate-tuning.
	GateEditTuning GateEditKind = "tuning"
	// GateEditLoosened means the gate still exists but no longer fails
	// closed the way it did: its fail branch was redirected away from
	// "@abort" (or made to converge with its own pass branch), so a
	// failing evaluation no longer blocks anything.
	GateEditLoosened GateEditKind = "loosened"
	// GateEditRemoved means the named gate no longer exists at all in the
	// new revision.
	GateEditRemoved GateEditKind = "removed"
)

// RequiresIndependentProof reports whether this classification is one TUT-A3
// forbids without independent proof the gate is dead.
func (k GateEditKind) RequiresIndependentProof() bool {
	return k == GateEditRemoved || k == GateEditLoosened
}

// ClassifyGateEdit compares gateName's definition across oldYAML (the gate's
// workflow file at the run's base ref) and newYAML (the same file in the
// drafted worktree), both raw goobers.dev/v1alpha1 Workflow documents. A gate
// absent from a workflow file simply isn't found there — callers scanning
// multiple changed workflow files should treat "not found in either" as
// GateEditNone and keep scanning other files before concluding a gate was
// never touched.
func ClassifyGateEdit(oldYAML, newYAML []byte, gateName string) (GateEditKind, error) {
	oldGate, oldErr := findGate(oldYAML, gateName)
	if oldErr != nil {
		return GateEditNone, fmt.Errorf("tutorguard: parse old workflow: %w", oldErr)
	}
	newGate, newErr := findGate(newYAML, gateName)
	if newErr != nil {
		return GateEditNone, fmt.Errorf("tutorguard: parse new workflow: %w", newErr)
	}

	switch {
	case oldGate == nil && newGate == nil:
		return GateEditNone, nil
	case oldGate != nil && newGate == nil:
		return GateEditRemoved, nil
	case oldGate == nil && newGate != nil:
		// The gate is newly introduced in this file — not the flagged gate's
		// removal/loosening, whatever else the diff does to it.
		return GateEditNone, nil
	}

	oldFailsClosed := failsClosed(*oldGate)
	newFailsClosed := failsClosed(*newGate)
	if oldFailsClosed && !newFailsClosed {
		return GateEditLoosened, nil
	}
	if gatesEqual(*oldGate, *newGate) {
		return GateEditNone, nil
	}
	return GateEditTuning, nil
}

// failsClosed reports whether the gate's failure branch still blocks: it
// routes somewhere distinct from the gate's own pass branch. A fail branch
// converging with pass no longer differentiates a failure from a success —
// the gate has been defeated regardless of what its target is named.
//
// This deliberately does NOT require the fail target to be the literal
// "@abort" terminal: most real shipped gates route a failure to a named
// repass/remediation state instead (e.g. implementation.yaml's `review` gate
// fails to "park-needs-human", `local-gate` fails to "implement") — neither
// aborts, but both still block the happy path exactly as surely as @abort
// does. Treating only "@abort" as blocking would make failsClosed report
// false for those gates even in their original, un-tampered state, so the
// oldFailsClosed && !newFailsClosed loosened-detection could never fire for
// them — a tutor redirecting exactly those gates' fail branch to converge
// with pass would then classify as ordinary tuning and skip the
// independent-proof requirement entirely (the metric-gaming path TUT-A3
// exists to block, on the gate-repass-churn finding kind most likely to name
// them).
func failsClosed(g apiv1.Gate) bool {
	fail, hasFail := g.Branches["fail"]
	if !hasFail || fail == "" {
		return false
	}
	pass, hasPass := g.Branches["pass"]
	return !hasPass || pass != fail
}

// gatesEqual does a structural, order-independent comparison via each gate's
// canonical JSON form (sigs.k8s.io/yaml round-trips through JSON already, so
// this reuses the same field set/tags the YAML parse honors).
func gatesEqual(a, b apiv1.Gate) bool {
	aj, aErr := yaml.Marshal(a)
	bj, bErr := yaml.Marshal(b)
	if aErr != nil || bErr != nil {
		return false
	}
	return bytes.Equal(aj, bj)
}

// findGate parses raw as a goobers.dev/v1alpha1 Workflow document and returns
// the Gate named name, or nil if the document has no such gate. A raw
// document that doesn't parse as YAML is an error; an empty/missing document
// (the file didn't exist at this revision) is not — callers pass nil bytes
// for "file absent at this revision" and get (nil, nil) back.
func findGate(raw []byte, name string) (*apiv1.Gate, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	var wf apiv1.Workflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		return nil, err
	}
	for i := range wf.Spec.Gates {
		if wf.Spec.Gates[i].Name == name {
			g := wf.Spec.Gates[i]
			return &g, nil
		}
	}
	return nil, nil
}
