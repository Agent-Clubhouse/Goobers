package netpolrender

import (
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
)

// BaselineUnit is the only unit a coverage baseline may declare. It is spelled
// out — and checked — because the unit IS the control: a baseline recorded in
// CIDR-block counts once produced a false-green (4/15 blocks looked like
// exclusion while covering 10,240 of 10,251 addresses), so a baseline file
// declaring any other unit is refused outright rather than reinterpreted.
const BaselineUnit = "addresses"

// Baseline is the committed per-class model-endpoint coverage snapshot the
// ratchet compares against (issue #3568 must-carry control 2). The check
// fails on a RISE only: coverage growing past the frozen number means a
// rotation or config change granted a restricted class address space nobody
// deliberately granted. A drop passes (with a note to re-freeze) — shrinking
// a grant is never a regression this control guards.
type Baseline struct {
	// Unit must be BaselineUnit.
	Unit string `json:"unit"`
	// Classes maps runner-class label values to their frozen entries.
	Classes map[string]BaselineEntry `json:"classes"`
}

// BaselineEntry is one class's frozen coverage.
type BaselineEntry struct {
	// Restrictions is the class's preimage (AnnotationRunnerClassRestrictions
	// value) — recorded so the baseline stays human-auditable when the class
	// value is an opaque hash.
	Restrictions string `json:"restrictions"`
	// ModelEndpointAddresses is the frozen address count, decimal-encoded
	// (big.Int — IPv6 counts exceed uint64).
	ModelEndpointAddresses string `json:"modelEndpointAddresses"`
}

// NewBaseline snapshots the current render's coverage as a baseline.
func NewBaseline(classes []Class, coverage map[string]*big.Int) Baseline {
	b := Baseline{Unit: BaselineUnit, Classes: make(map[string]BaselineEntry, len(classes))}
	for _, class := range classes {
		b.Classes[class.Value] = BaselineEntry{
			Restrictions:           class.Preimage,
			ModelEndpointAddresses: coverage[class.Value].String(),
		}
	}
	return b
}

// MarshalBaseline renders a baseline as stable, committed JSON.
func MarshalBaseline(b Baseline) ([]byte, error) {
	out, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// ParseBaseline decodes a committed baseline, refusing unknown fields and any
// unit other than addresses.
func ParseBaseline(raw []byte) (Baseline, error) {
	var b Baseline
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return Baseline{}, fmt.Errorf("parse coverage baseline: %w", err)
	}
	if b.Unit != BaselineUnit {
		return Baseline{}, fmt.Errorf("coverage baseline declares unit %q; the only accepted unit is %q — "+
			"block counts flatter exclusion (aggregates are the only non-/32 entries) and produced a real false-green",
			b.Unit, BaselineUnit)
	}
	return b, nil
}

// CheckBaseline compares the current render's coverage against the frozen
// baseline. It returns an error when any class's coverage ROSE or is not
// frozen at all (an unfrozen class silently passing would be the
// silent-toward-passing defect this control exists to close), and notes for
// conditions worth telling the operator about without failing.
func CheckBaseline(baseline Baseline, classes []Class, coverage map[string]*big.Int) (notes []string, err error) {
	var failures []string
	for _, class := range classes {
		current := coverage[class.Value]
		entry, frozen := baseline.Classes[class.Value]
		if !frozen {
			failures = append(failures, fmt.Sprintf(
				"class %s [%s] has no frozen baseline entry — freeze it deliberately with --write-baseline",
				class.Value, class.Preimage))
			continue
		}
		frozenCount, ok := new(big.Int).SetString(entry.ModelEndpointAddresses, 10)
		if !ok {
			failures = append(failures, fmt.Sprintf(
				"class %s baseline value %q is not a decimal address count", class.Value, entry.ModelEndpointAddresses))
			continue
		}
		switch current.Cmp(frozenCount) {
		case 1:
			failures = append(failures, fmt.Sprintf(
				"class %s [%s] model-endpoint coverage ROSE: %s addresses now vs %s frozen — "+
					"a rotation or config change granted address space nobody deliberately granted; "+
					"review the diff, then re-freeze with --write-baseline if intended",
				class.Value, class.Preimage, current.String(), frozenCount.String()))
		case -1:
			notes = append(notes, fmt.Sprintf(
				"class %s coverage dropped (%s addresses now vs %s frozen) — passing; re-freeze with --write-baseline to ratchet down",
				class.Value, current.String(), frozenCount.String()))
		}
	}
	rendered := make(map[string]bool, len(classes))
	for _, class := range classes {
		rendered[class.Value] = true
	}
	var stale []string
	for value := range baseline.Classes {
		if !rendered[value] {
			stale = append(stale, value)
		}
	}
	sort.Strings(stale)
	for _, value := range stale {
		notes = append(notes, fmt.Sprintf("baseline entry %s matches no rendered class — remove it with --write-baseline", value))
	}
	if len(failures) > 0 {
		return notes, fmt.Errorf("coverage ratchet failed (%d):\n  %s", len(failures), strings.Join(failures, "\n  "))
	}
	return notes, nil
}
