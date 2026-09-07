# Deadcode exemption policy

`make ci` runs this gate before analysis. New or edited exemptions must cite an
open owning issue (`#123` or `(#123)`) or carry a separate
`expires=YYYY-MM-DD` field in their reason. Expiries are valid through the named
UTC date; an expired or malformed expiry fails even if an open issue is cited.
Several issue references are allowed: at least one must be open. Cross-repository
`owner/repo#123` references do not count as local owners.

The default check is offline, using `issue-states.json`. Its repository and
observation date are explicit; absent issue evidence is unknown, never guessed
open. To cite a new owner, verify its current state with
`gh issue view NUMBER --repo Agent-Clubhouse/Goobers --json number,state`, then
update the committed snapshot in the same PR. Refresh existing entries from the
same repository when they close. Merged PR references are recorded as closed.
A snapshot older than 30 days warns that its evidence needs refreshing.
`-issue-states PATH` selects another reviewed snapshot with the same format.

The snapshot was verified on 2026-09-07. It is cached evidence, not a claim of
live access on every CI run. There are no network calls or credential
requirements in the default gate.

`policy-baseline.txt` freezes the exact pre-existing exemption entries from
41801d7d8, so this policy can land before the separate legacy-ledger cleanup.
The embedded baseline is digest-checked; do not add new entries to it. A new
symbol, changed reason, or changed platform qualifier cannot inherit an old
exception. Existing policy violations are counted on every run; use
`go run ./test/deadcode -policy-details` to list each symbol and its violation.
This rollout exception never suppresses the existing stale-symbol or
platform-reachability checks.

The baseline is a fixed migration record, not a second live exemption list.
Remove or repair entries in `exemptions.txt`; no baseline additions are needed
for that cleanup.
