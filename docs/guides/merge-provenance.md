# Merge provenance reports

Forge identity-based merge counting is **not authoritative daemon attribution**.
A human, another tool, and multiple Goobers instances can share one account.
PR author and branch prefix are not evidence of who performed a landing.

`goobers telemetry merges` queries retained, explicit landing receipts and their
immutable originating instance identity. It counts unique PRs per repository,
gaggle, instance and UTC day. Legacy `operation=merge` observations without a
confirmation are not verified. Conflicting instance, gaggle or commit claims
are excluded before display filters. Rebuilding only recovers retained journals;
it cannot reconstruct receipts that were never recorded or have been pruned.

```sh
goobers telemetry merges --json --since=2026-09-01T00:00:00Z --until=2026-09-08T00:00:00Z /path/to/instance
```

## GitHub residual comparison

Provision a repository-scoped token with **Pull requests: Read-only** in
`GOOBERS_CRED_GITHUB_PR_READ` through your secret manager or shell environment.
Do not put the token on the command line. No write-token fallback is used.
The `github:pr:read` capability is explicit-only: a stage needs its own declared
grant and credential source, not an inherited daemon mutation credential.

```sh
goobers telemetry merges --json --compare-github=acme/app --shared-identities=shared-login --since=2026-09-01T00:00:00Z --until=2026-09-08T00:00:00Z /path/to/instance
```

The optional comparison contains individual records and daily counts:

- `daemon-verified`: an unambiguous retained receipt matches the repository,
  PR and, when both provide it, merge commit. Instance and gaggle come from that
  receipt, not the forge account.
- `same-identity-unverified`: the actual merger matches one of the explicitly
  supplied, comma-separated shared logins, but no matching receipt verifies it.
  This does **not** prove a human merged it.
- `external`: the forge identifies a different merger.
- `merger-unknown`: the forge supplies no merger identity. It is not counted as
  external and is never replaced with the PR author.

Comparison residuals are repository-wide, independent of `--gaggle`,
`--instance-id` and `--repository-api-url` display filters. An unverified merge
has no known gaggle. Proof from another retained fleet is kept distinct rather
than discarded by a display filter. Daily comparison counts use the forge's
merge timestamp, not the PR's last-update timestamp.

## Bounds and coverage

The interval includes `--since` and excludes `--until`. The default is seven
days; the maximum is 90 days. More than 10,000 telemetry events or raw forge
records causes an error, not a partial count. Forge scanning also has a
101-page and two-minute bound. Malformed pages, repeated PRs, inconsistent
list/detail responses and provider errors fail the comparison.

GitHub pagination is not an atomic snapshot: concurrent repository updates can
move records between pages. Duplicate detection catches some, not all, such
changes. Retention and receipt-time boundaries can leave genuine daemon merges
in the unverified residual. Treat the coverage label and unknown counts as part
of the report, not optional footnotes. This comparison currently supports GitHub;
the retained telemetry view accepts GitHub, Azure DevOps and Gitea receipts.

Queue-completion attribution and crash recovery remain under development in
#3019; the presence of this report does not establish that every landing path
emits a durable confirmation.
