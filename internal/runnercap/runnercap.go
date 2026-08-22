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
	"strings"
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

// Derived-requirement vocabulary (dsl-3.0.md D7, decision record D2): the
// placement tags a stage carries by construction rather than by declaration —
// "harness:<name>" for agentic stages (from the goober's harness: field) and
// "shell" for sh/make stages. This leaf package owns the spellings so the
// deriving side (internal/workflow/v_3_0) and the matching side
// (internal/runnersolve) cannot drift. Note "harness:<name>" deliberately
// fails ValidToken: it is a system-derived fact, not an author token, so a
// runner cannot claim it in provides.capabilities — how a non-self runner
// image advertises a harness is the dispatcher/image contract's to define
// (#3513, decision record D8); in v1 only the self runner satisfies harness
// tags (implicitly — the daemon host runs every configured harness through
// the local execution path, preflight-verified at startup).
const (
	// DerivedShellTag is the derived requirement of a stage that shells out.
	DerivedShellTag = "shell"
	// DerivedHarnessTagPrefix prefixes the derived requirement of an agentic
	// stage: DerivedHarnessTagPrefix + the goober's harness name.
	DerivedHarnessTagPrefix = "harness:"
)

// DerivedTag reports whether s is a member of the derived-requirement
// namespace ("shell", or any "harness:"-prefixed tag).
func DerivedTag(s string) bool {
	return s == DerivedShellTag || strings.HasPrefix(s, DerivedHarnessTagPrefix)
}

// Restriction is one isolation effect from the closed v1 effect list
// (Goobernetes decision record D7, docs/design/goobernetes-restrictions.md
// §2). Restrictions name effects, never mechanisms; growing this set is a
// product decision recorded there, not a config-side addition. This is the
// single vocabulary shared by the instance runners: inventory
// (internal/instance) and the DSL 3.0 runsOn.restrictions surface
// (internal/workflow/v_3_0), which cannot import each other's packages.
type Restriction string

// The closed v1 restriction effect list.
const (
	RestrictionNetworkNone      Restriction = "network:none"
	RestrictionNetworkAllowlist Restriction = "network:allowlist"
	RestrictionFSReadonly       Restriction = "fs:readonly-except-workspace"
	RestrictionTmpEphemeral     Restriction = "tmp:ephemeral"
	RestrictionEnvDefaultDeny   Restriction = "env:default-deny"
)

// knownRestrictions is the closed-list membership check, in stable order.
var knownRestrictions = []Restriction{
	RestrictionEnvDefaultDeny,
	RestrictionFSReadonly,
	RestrictionNetworkAllowlist,
	RestrictionNetworkNone,
	RestrictionTmpEphemeral,
}

// KnownRestrictions returns the closed v1 restriction effect list, sorted for
// stable diagnostics.
func KnownRestrictions() []Restriction {
	return append([]Restriction(nil), knownRestrictions...)
}

// KnownRestriction reports whether s is a member of the closed effect list.
func KnownRestriction(s string) bool {
	for _, r := range knownRestrictions {
		if string(r) == s {
			return true
		}
	}
	return false
}

// SuggestRestriction returns the closest known restriction to a token that is
// not one (edit distance at most 3 — restriction effects are longer than
// credential capability names, so the internal/capability threshold of 2
// would miss plausible typos like "network:allow-list"). ok is false when s
// is already known or nothing is plausibly close.
func SuggestRestriction(s string) (Restriction, bool) {
	if KnownRestriction(s) {
		return "", false
	}
	bestDistance := -1
	var best Restriction
	for _, candidate := range knownRestrictions {
		distance := editDistance(s, string(candidate))
		if bestDistance == -1 || distance < bestDistance {
			bestDistance = distance
			best = candidate
		}
	}
	if bestDistance > 3 {
		return "", false
	}
	return best, true
}

// editDistance is the Levenshtein distance between a and b (mirrors
// internal/capability's; duplicated because this package stays stdlib-only).
func editDistance(a, b string) int {
	previous := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current := make([]int, len(b)+1)
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			current[j] = min(
				current[j-1]+1,
				previous[j]+1,
				previous[j-1]+cost,
			)
		}
		previous = current
	}
	return previous[len(b)]
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
