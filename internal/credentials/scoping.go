package credentials

// RepoBinding maps a target repository (by owner/name) to the resolver token-ref
// name that backs it. It is the input to per-gaggle credential scoping (MGV-5,
// #1012): the runner computes one gaggle's grants from the bindings of the
// instance's repos plus that gaggle's own project repo.
type RepoBinding struct {
	Owner    string
	Name     string
	TokenRef string
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
// overrides source individual capabilities from their own refs (#287 — e.g.
// agent:model from a personal token): an override for a capability the repo
// token would otherwise back REPLACES that grant, and a new capability is added.
// These stay unqualified (shared) — the agent-model token every gaggle uses.
//
// Grant order is deterministic: the repo-capability defaults first (in
// credentialedCaps order), then any override-only capabilities (in overrides
// order), so the resulting grant slice is stable across builds.
func RunnerGrants(bindings []RepoBinding, owner, name string, credentialedCaps []string, overrides []Grant) []Grant {
	defaultRef := ""
	if len(bindings) > 0 {
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

	grantRef := make(map[string]string, len(credentialedCaps)+len(overrides))
	order := make([]string, 0, len(credentialedCaps)+len(overrides))
	if defaultRef != "" {
		for _, c := range credentialedCaps {
			if _, exists := grantRef[c]; !exists {
				order = append(order, c)
			}
			grantRef[c] = defaultRef
		}
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
