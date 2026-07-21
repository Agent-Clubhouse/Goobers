// Package runnercap is the vocabulary and matching logic for a runner's
// (toolchain/platform) capability claims and the requirements a gaggle or stage
// declares against them (RRQ-1, issue #1101,
// docs/design/v1/polyglot-stacks.md §5).
//
// These are deliberately NOT the credential capabilities of
// internal/capability (`repo:push`, `agent:model`, &c.): those name a scoped
// grant a goober holds, drawn from a closed canonical registry the DSL compiler
// validates against. A runner capability instead names an installed toolchain
// or host property — `dotnet@8`, `netfx@4.8`, `xcode`, `os=windows` — which is
// open-ended and version-parameterized by design (the reference version is
// incidental; swapping it is a config change, not a code change). So there is
// no closed enum here: validation is a well-formedness check on the token
// shape, and satisfiability is a set-membership check of required-against-
// claimed. The PO-confirmed model (2026-07-20): assume the toolchain is
// preinstalled; a runner advertises a static capability set; a gaggle/stage
// declares what it requires; the scheduler fails a workload to schedule when a
// requirement is unmet — it does NOT install anything, and a runner that
// falsely claims a capability it lacks degrades to a runtime error the
// scheduler does not prevent.
//
// The package has no dependencies beyond the stdlib, so every layer that names
// a runner capability — the instance-config validator, the config-as-code
// cross-check, and the scheduler's admit path — can depend on it without
// pulling in anything heavier.
package runnercap

import (
	"fmt"
	"regexp"
)

// tokenPattern bounds a well-formed capability token: it must start with an
// alphanumeric and then carry only alphanumerics and the small set of
// separators the design's examples use — `.` and version `@`
// (`dotnet@8.0`), `=` (`os=windows`), plus `-`/`_`/`+` for names like
// `x86_64`/`build-tools`/`c++`. No whitespace, so a token is always a single
// grep-able word in a diagnostic.
var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@=+-]*$`)

// ValidToken reports whether s is a well-formed runner-capability token.
func ValidToken(s string) bool {
	return tokenPattern.MatchString(s)
}

// ValidateToken returns a descriptive error when s is not a well-formed
// capability token, and nil otherwise. Callers surface it at config-load so a
// malformed claim or requirement fails closed rather than silently never
// matching.
func ValidateToken(s string) error {
	if s == "" {
		return fmt.Errorf("capability must not be empty")
	}
	if !ValidToken(s) {
		return fmt.Errorf("capability %q is malformed (allowed: an alphanumeric start then letters, digits, and any of . _ - + @ =, no whitespace)", s)
	}
	return nil
}

// Claimed is a runner's advertised capability set, built once from config and
// queried on every admit. The zero value claims nothing.
type Claimed map[string]struct{}

// NewClaimed builds a Claimed set from a runner's declared capability tokens.
// Duplicates collapse; the tokens are stored verbatim (an exact string match is
// what schedule-time matching uses, so `dotnet@8` and `dotnet@10` are distinct
// capabilities, never a range).
func NewClaimed(caps []string) Claimed {
	set := make(Claimed, len(caps))
	for _, c := range caps {
		set[c] = struct{}{}
	}
	return set
}

// Has reports whether the runner claims capability c.
func (c Claimed) Has(cap string) bool {
	_, ok := c[cap]
	return ok
}

// Missing returns the required capabilities the runner does not claim, in the
// order they first appear in required and de-duplicated, so a diagnostic lists
// each gap once and stably. An empty result means every requirement is met (and
// an empty required set is trivially met, so a workload that declares no
// requirement is never refused).
func (c Claimed) Missing(required []string) []string {
	var missing []string
	seen := make(map[string]struct{}, len(required))
	for _, r := range required {
		if _, dup := seen[r]; dup {
			continue
		}
		seen[r] = struct{}{}
		if !c.Has(r) {
			missing = append(missing, r)
		}
	}
	return missing
}
