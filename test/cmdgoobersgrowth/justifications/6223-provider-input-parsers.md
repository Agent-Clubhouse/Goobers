# #6223: shared bounded provider-input parsing helpers

Growth: +1 non-test file in `cmd/goobers` (`providerinputparse.go`) and about
+94 non-test lines.

## Why the growth belongs in the command package

`providerinputparse.go` holds four small parse-and-validate helpers
(`parseIntInput`, `parseFloatInput`, `parseDurationInput`, `parseBoolInput`)
that seven provider-stage command readers (`backlog-dedupe`, `backlog-health`,
`backlog-query`'s policy, re-sweep and staleness readers,
`docs-churn`, `file-issues`) now use instead of hand-rolled
`strconv` + range-check + error blocks. Each caller keeps its own default,
range predicate, exact error text and exit code, which is why the call sites
pass a predicate and an error-text callback and why the line count grows
rather than shrinks.

The helpers take the raw value, not the input name: every input is still read
through a literal `providerInput("<field>", default)` call at the call site so
`internal/providerstage`'s schema audit keeps discovering it.

## Could any of it live elsewhere?

The helpers are generic enough for a small internal package, but their only
consumers are `cmd/goobers` stage commands and they exist to keep those
commands' diagnostics byte-identical. The growth is the price of a uniform
parse/validate shape, not new behaviour.
