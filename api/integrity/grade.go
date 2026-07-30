// Package integrity defines the provenance grades shared by provider reads,
// workflow contracts, invocation envelopes, and run journals.
package integrity

// Grade identifies the provenance of content crossing a stage boundary.
type Grade string

// Provenance grades ordered from operator-controlled through arbitrary input,
// with agent output distinguished as derived content.
const (
	Trusted    Grade = "trusted"
	Maintainer Grade = "maintainer"
	Unapproved Grade = "unapproved"
	Derived    Grade = "derived"
)

// Valid reports whether g is a declared integrity grade.
func (g Grade) Valid() bool {
	switch g {
	case Trusted, Maintainer, Unapproved, Derived:
		return true
	default:
		return false
	}
}

// Meets reports whether g satisfies minimum. Derived workflow output is admitted
// at the maintainer tier while remaining distinguishable from maintainer-authored
// input; only operator/config input satisfies a trusted minimum.
func (g Grade) Meets(minimum Grade) bool {
	if !g.Valid() || !minimum.Valid() {
		return false
	}
	return level(g) >= level(minimum)
}

// Weakest returns the least trustworthy provenance in grades. Invalid or empty
// input returns the zero grade so callers can fail closed.
func Weakest(grades ...Grade) Grade {
	if len(grades) == 0 {
		return ""
	}
	weakest := grades[0]
	if !weakest.Valid() {
		return ""
	}
	for _, grade := range grades[1:] {
		if !grade.Valid() {
			return ""
		}
		if provenanceLevel(grade) < provenanceLevel(weakest) {
			weakest = grade
		}
	}
	return weakest
}

func level(g Grade) int {
	switch g {
	case Trusted:
		return 2
	case Maintainer, Derived:
		return 1
	default:
		return 0
	}
}

func provenanceLevel(g Grade) int {
	switch g {
	case Trusted:
		return 3
	case Maintainer:
		return 2
	case Derived:
		return 1
	default:
		return 0
	}
}
