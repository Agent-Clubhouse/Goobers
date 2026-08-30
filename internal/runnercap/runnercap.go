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
// "run:shell" for sh/make stages. This leaf package owns the spellings so the
// deriving side (internal/workflow/v_3_0) and the matching side
// (internal/runnersolve) cannot drift. EVERY derived tag deliberately fails
// ValidToken (the colon is rejected by the author-token grammar): a derived
// tag is a system-derived fact an author can neither require nor claim in
// provides.capabilities — how a non-self runner image advertises a harness
// or shell is the dispatcher/image contract's to define (#3513, decision
// record D8); in v1 only the self runner satisfies derived tags (implicitly
// — the daemon host runs every configured harness and every shell stage
// through the local execution path, preflight-verified at startup).
const (
	// DerivedShellTag is the derived requirement of a stage that shells out.
	// Spelled with a colon namespace ("run:", matching the harness:<name>
	// pattern) precisely so it lives OUTSIDE the author grammar: a plain
	// "shell" token passes ValidToken, so an author CAN spell it, and an
	// author-spelled token must behave as an ordinary capability — exact set
	// membership, never the self-implicit derived-tag satisfaction. A 2.0
	// config declaring requiredCapabilities: [shell] therefore admits
	// byte-identically to legacy runnercap matching on every instance shape.
	DerivedShellTag = "run:shell"
	// DerivedHarnessTagPrefix prefixes the derived requirement of an agentic
	// stage: DerivedHarnessTagPrefix + the goober's harness name.
	DerivedHarnessTagPrefix = "harness:"
)

// DerivedTag reports whether s is a member of the derived-requirement
// namespace ("run:shell", or any "harness:"-prefixed tag). Membership keys
// ONLY on the colon-namespaced spellings above — every member fails the
// author-token grammar by design, so no author-spellable token (including
// the plain "shell") is ever a derived tag.
func DerivedTag(s string) bool {
	return s == DerivedShellTag || strings.HasPrefix(s, DerivedHarnessTagPrefix)
}

// CapabilityWindowsAdmin is the ONE capability token the product itself
// interprets (issue #3619): a claim, by a Windows runner class, that stages
// placed on it may run as the container's administrator identity
// (ContainerAdministrator), and a requirement, by a stage, that it needs to.
//
// It is a CAPABILITY, not a restriction, on purpose. A restriction names an
// isolation effect the substrate takes away; this names something the
// substrate OFFERS — the same shape as `dotnet@8`: the runner claims it in
// provides.capabilities, the stage requires it in runsOn.capabilities, and
// the solver matches by exact set membership like every other token, so a
// stage requiring it places ONLY on a class that provides it. What is new is
// the binding: the dispatcher stamps windowsOptions.runAsUserName
// ContainerAdministrator on a Windows stage pod when — and only when — the
// stage REQUIRES the token and the resolved runner PROVIDES it, and stamps
// ContainerUser otherwise. Provided-but-not-required stays ContainerUser
// (least privilege is the default in both directions); required-but-not-
// provided is refused at dispatch, never silently served.
//
// Spelled with `=` rather than a colon deliberately: the colon namespace is
// the DERIVED-tag grammar (DerivedTag — system-derived facts an author can
// neither claim nor require), and this token must be author-declarable on
// both sides. `=` is the same separator the legacy `os=windows` token used;
// CAP004 bans only the `os=` prefix, and the toolchain preflight has no
// prober for the `privilege` family, so the token is inert everywhere except
// the three sites that read it by this constant (the 3.0 validator, the
// instance inventory loader, and the dispatcher's identity stamp).
const CapabilityWindowsAdmin = "privilege=windows-admin"

// HasWindowsAdmin reports whether tokens (a stage's effective requirement or
// a runner's claim set) contains CapabilityWindowsAdmin.
func HasWindowsAdmin(tokens []string) bool {
	for _, t := range tokens {
		if t == CapabilityWindowsAdmin {
			return true
		}
	}
	return false
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

// windowsDeclarable is the sub-list of the closed effect list a WINDOWS
// runner may declare (and a Windows-placed stage may require) in v1 —
// goobernetes-restrictions.md D4 as corrected by #3619. Windows can bind
// exactly two effects today: tmp:ephemeral (the dispatcher mounts a sized
// emptyDir at the profile temp path and points TMP/TEMP at it) and
// env:default-deny (the in-pod procenv rebuild is OS-independent). The other
// three have NO Windows binding: Kubernetes silently ignores
// readOnlyRootFilesystem on a Windows pod (fails OPEN — decision 007), and
// the network effects are NetworkPolicy-class bindings D11's epic has yet to
// verify on Windows nodes. A restriction a runner cannot enforce must be
// UNDECLARABLE, or validation produces confident PASSes on unenforced
// substrate; every member of the closed list has a decided answer here and
// TestDeclarableOnWindowsCoversClosedList pins it.
var windowsDeclarable = map[Restriction]bool{
	RestrictionTmpEphemeral:   true,
	RestrictionEnvDefaultDeny: true,
}

// DeclarableOnWindows reports whether restriction r has a Windows binding in
// v1 — whether a Windows runner may declare it and a Windows-placed stage may
// require it. Unknown effects are not declarable anywhere. Consumed by three
// sites that must agree: the DSL 3.0 validator (a Windows-placed stage
// requiring an unbindable effect is refused at validate), the instance
// inventory loader (a Windows runner declaring one is refused at load), and
// the dispatcher (re-asserted at pod render, refuse-to-create).
func DeclarableOnWindows(r Restriction) bool {
	return windowsDeclarable[r]
}

// WindowsDeclarableRestrictions returns the effects a Windows runner may
// declare in v1, in stable order, for diagnostics.
func WindowsDeclarableRestrictions() []Restriction {
	out := make([]Restriction, 0, len(windowsDeclarable))
	for _, r := range knownRestrictions {
		if windowsDeclarable[r] {
			out = append(out, r)
		}
	}
	return out
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
