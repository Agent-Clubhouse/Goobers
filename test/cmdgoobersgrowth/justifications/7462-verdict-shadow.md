# Missing-verdict shadow observation (#7462)

The command adds a small adapter between its existing locked provider poll and
the instance annotation seam. Only `merge-pr` knows whether all current gates
allowed a landing attempt, so that production wiring belongs in `cmd/goobers`.
Provider evidence classification and the would-block predicate live in
`internal/prstatus`. This slice changes no enforcement and adds no durable store:
the existing instance journal retention loop bounds its annotations.
