# #2435 part 1: inject open-PR listers via runtimeDeps

The command package gains `runtimedeps.go` and about 40 non-test lines. They
add `runtimeDeps`, the immutable value of runtime factories with production
defaults that replaces the package-level function vars tests reassigned, and
move its first family into it: the GitHub and ADO open-PR provider factories.

This code is daemon composition-root wiring. It decides which concrete forge
clients the daemon's scheduler and runners are built with, and it has to name
types that only exist in `cmd/goobers`: `resolvingOpenPRLister`,
`adoOpenPRLister` and the constructors that take them. Moving it to `internal/`
would mean moving those listers and their callers too, which is outside this
refactor. Most of the growth is doc comments plus the factory fields that
replace the two deleted package vars. The PR adds no runtime logic.
