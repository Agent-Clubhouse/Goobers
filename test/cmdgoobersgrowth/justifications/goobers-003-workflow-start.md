GOOBERS-003 adds operator-facing workflow start safety in `cmd/goobers` so
remote starts require a revision-pinned request and report only durable run
identities as successful outcomes. The extra CLI lines live in `cmd/goobers`
because they parse and validate operator flags, preserve retry identity, and
translate daemon API results into the command's operator-facing exit contract.
