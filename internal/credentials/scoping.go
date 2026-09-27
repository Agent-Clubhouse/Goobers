package credentials

import "strings"

// RepoBinding maps a target repository (by owner/name) to the resolver token-ref
// name that backs it. It is the input to per-gaggle credential scoping (MGV-5,
// #1012): the runner computes one gaggle's grants from the bindings of the
// instance's repos plus that gaggle's own project repo.
type RepoBinding struct {
	Owner    string
	Name     string
	TokenRef string
}

// BacklogRole names the repository that backs a gaggle's backlog-role
// capabilities when its backlog lives on a different provider than its
// project (topology (b), docs/design/ado-parity-dsl-2-0.md §7.2). Owner and
// Name select the binding by the gaggle's backlog repository; Capabilities is
// the capability family that routes to the backlog provider (§3.1).
type BacklogRole struct {
	Owner        string
	Name         string
	Capabilities []string
}

// RunnerGrants computes the runner-owned credential grants for a gaggle whose
// project repo is (owner, name). Every capability in credentialedCaps is granted
// that gaggle's own repo token — the binding whose Owner/Name match — so a
// gaggle's stages only ever hold a token for that gaggle's repo, not a shared
// instance-wide one (per-repo credential scoping, docs/design/v1/
// multi-gaggle-validation.md §G1). When no binding matches (a single-repo or
// legacy instance, or an unqualified caller), the FIRST binding backs the repo
// capabilities — byte-identical to the pre-scoping "first repo's token backs
// every credentialed capability" default, so a one-gaggle instance is unchanged.
//
// backlog is the role-aware binding choice, nil for every gaggle whose backlog
// shares its project's provider. With a backlog role, each credentialed
// capability in backlog.Capabilities is granted the binding matching the
// backlog repository's owner/name instead of the project repo's, and every
// other capability keeps the project binding. The two repositories are on
// different providers, so a backlog capability is never backed by the project
// repo's credential: when no binding with a token matches the backlog
// repository, the capability gets no grant and a stage that declares it fails
// closed for want of a credential, rather than sending one provider's
// credential to the other. For the same reason a backlog role turns off the
// first-binding fallback for every other capability: when no binding matches
// the project repository exactly, those capabilities get no grant rather than
// whichever repository happens to be listed first.
//
// overrides source individual capabilities from their own refs (#287 — e.g.
// agent:model from a personal token): an override for a capability the repo
// token would otherwise back REPLACES that grant, and a new capability is added.
// These stay unqualified (shared) — the agent-model token every gaggle uses.
//
// Grant order is deterministic: the repo-capability defaults first (in
// credentialedCaps order), then any override-only capabilities (in overrides
// order), so the resulting grant slice is stable across builds.
func RunnerGrants(bindings []RepoBinding, owner, name string, backlog *BacklogRole, credentialedCaps []string, overrides []Grant) []Grant {
	defaultRef := ""
	// The first-binding fallback is for single-provider instances only. With a
	// backlog role the first binding may be the backlog repository, on the
	// other provider, so the project family is backed only by an exact match.
	if len(bindings) > 0 && backlog == nil {
		defaultRef = bindings[0].TokenRef
	}
	if owner != "" && name != "" {
		for _, b := range bindings {
			if b.Owner == owner && b.Name == name {
				defaultRef = b.TokenRef
				break
			}
		}
	}

	backlogRef, backlogCaps := backlogRoleRef(bindings, backlog)
	grantRef := make(map[string]string, len(credentialedCaps)+len(overrides))
	order := make([]string, 0, len(credentialedCaps)+len(overrides))
	for _, c := range credentialedCaps {
		ref := defaultRef
		if backlogCaps[c] {
			ref = backlogRef
		}
		if ref == "" {
			continue
		}
		if _, exists := grantRef[c]; !exists {
			order = append(order, c)
		}
		grantRef[c] = ref
	}
	for _, o := range overrides {
		if _, exists := grantRef[o.Capability]; !exists {
			order = append(order, o.Capability)
		}
		grantRef[o.Capability] = o.Ref
	}

	grants := make([]Grant, 0, len(order))
	for _, c := range order {
		grants = append(grants, Grant{Capability: c, Ref: grantRef[c]})
	}
	return grants
}

// backlogRoleRef resolves the token ref backing a backlog role and the set of
// capabilities it backs. It returns an empty ref (no grant) when no binding
// with a token matches the backlog repository, and no capabilities for a nil
// role.
func backlogRoleRef(bindings []RepoBinding, backlog *BacklogRole) (string, map[string]bool) {
	if backlog == nil {
		return "", nil
	}
	caps := make(map[string]bool, len(backlog.Capabilities))
	for _, c := range backlog.Capabilities {
		caps[c] = true
	}
	for _, b := range bindings {
		if b.TokenRef != "" && b.Owner == backlog.Owner && b.Name == backlog.Name {
			return b.TokenRef, caps
		}
	}
	return "", caps
}

// RepoScopedCapability returns the repo-qualified grant key for a base capability
// against one repository: "base@owner/name". It carries the repo dimension of the
// (gaggle, repo, capability) scope key through the otherwise repo-agnostic
// capability→token maps (docs/design/v1/multi-gaggle-validation.md §2), so a
// single capability can resolve to a DIFFERENT token per repo — e.g. one read
// token per reference repo. The key is opaque to Injector/Set, which treat all
// capability strings uniformly.
func RepoScopedCapability(base, owner, name string) string {
	return base + "@" + owner + "/" + name
}

// harnessScopeSeparator namespaces a harness-scoped grant capability key away
// from any base capability string. Capability strings are colon-separated
// (e.g. "agent:model") and repo-scoped keys use "@" (RepoScopedCapability), so
// "#harness:" cannot collide with either.
const harnessScopeSeparator = "#harness:"

// HarnessScopedCapability returns the harness-qualified grant key for a base
// capability: "base#harness:name". It carries the harness dimension of a
// credentialGrant's optional harness selector (#5148) through the otherwise
// harness-agnostic capability→token maps, mirroring RepoScopedCapability's
// repo dimension. The key is opaque to Injector/Set, which treat all
// capability strings uniformly; only the wiring that binds a goober's own
// harness to its credential sources (buildGooberCredentialGrants) ever splits
// it back apart, collapsing to a plain capability key before a Set is
// materialized — so nothing downstream of that point needs to know harness
// scoping exists.
func HarnessScopedCapability(base, harness string) string {
	return base + harnessScopeSeparator + harness
}

// SplitHarnessScopedCapability reverses HarnessScopedCapability. ok is false
// for a plain (unscoped) capability key, in which case base is returned
// unchanged and harness is "".
func SplitHarnessScopedCapability(key string) (base, harness string, ok bool) {
	i := strings.Index(key, harnessScopeSeparator)
	if i < 0 {
		return key, "", false
	}
	return key[:i], key[i+len(harnessScopeSeparator):], true
}

// AdditionalReadGrants computes runner-owned read-only grants for a gaggle's
// additional reference repos (MGV-10, #1285). For each additional repo that has a
// token-bearing binding, it emits one grant keyed by RepoScopedCapability(
// readCapability, owner, name) → that repo's own token ref. A reference repo with
// no configured token is skipped (nothing to route). No write capability is ever
// produced here: an AdditionalRepos entry is read-only by construction, so a
// stage can never obtain a write token for it (there is none to grant). The
// grants are runner-owned (empty Goober) — they authenticate the reference-repo
// checkout at provision time and are never bound into a goober's stage injector.
//
// additional carries the reference repos' (Owner, Name); its TokenRef field is
// ignored — the token is looked up from bindings (the instance's configured
// repos) by owner/name, so a reference repo must appear in the instance repo list
// with its own scoped read token. Grant order follows additional's order for
// determinism.
func AdditionalReadGrants(bindings []RepoBinding, additional []RepoBinding, readCapability string) []Grant {
	if readCapability == "" {
		return nil
	}
	tokenByRepo := make(map[string]string, len(bindings))
	for _, b := range bindings {
		if b.TokenRef != "" {
			tokenByRepo[b.Owner+"/"+b.Name] = b.TokenRef
		}
	}
	grants := make([]Grant, 0, len(additional))
	seen := make(map[string]bool, len(additional))
	for _, a := range additional {
		key := a.Owner + "/" + a.Name
		if seen[key] {
			continue
		}
		ref, ok := tokenByRepo[key]
		if !ok {
			continue
		}
		seen[key] = true
		grants = append(grants, Grant{Capability: RepoScopedCapability(readCapability, a.Owner, a.Name), Ref: ref})
	}
	return grants
}
