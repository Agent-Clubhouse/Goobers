package capability

// subsumptions declares, per held capability, the strictly-narrower
// capabilities that grant also satisfies. It exists because admission is
// exact-set-membership (internal/workflow/v_current/compile.go's
// admissionProblems, mirrored in v_2_0): when TBH-1 (#2386, commit ed11ae81)
// NARROWED backlog-dedupe's requirement from github:issues:write to
// github:issues:read, every existing config that declared the broader write
// grant stopped compiling — a strictly-safer change broke workflows that
// already held strictly-more authority than the new requirement asked for.
//
// The table is a deliberate, reviewed enumeration — never string
// pattern-matching on the "resource:verb" shape — because ADR 0002 forbids
// implicit capability aliases: which grant satisfies which requirement is a
// security statement, and every pair must be individually argued for in
// review. Tests pin the table's exact size so an addition cannot ride in
// unnoticed.
var subsumptions = map[Capability][]Capability{
	// A grant that may create/label/close/comment on issues can do everything
	// a read-only issue query can; requiring read must not reject holders of
	// write (the #2386 breakage class).
	GitHubIssuesWrite: {GitHubIssuesRead},
}

// Subsumes reports whether holding held satisfies a requirement for required:
// either they are the same capability, or the subsumptions table explicitly
// declares held as a broader grant covering required. Callers pass canonical
// capabilities; membership validation stays with Known/StageDeclarable.
//
// Not yet consumed by admission — compile.go still checks exact membership.
// Wiring the relation into the WF010 check is a follow-up wave; this package
// only defines the relation so that wave is a call-site change, not a policy
// decision.
func Subsumes(held, required Capability) bool {
	if held == required {
		return true
	}
	for _, narrower := range subsumptions[held] {
		if narrower == required {
			return true
		}
	}
	return false
}
