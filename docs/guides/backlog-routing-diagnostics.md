# Find ready issues with no label route

For lease/provider drift rather than label routing, see
[claim verification](claim-verification.md).

For the curation park-label opt-out, see [park filtering](backlog-park-filtering.md).

Run `goobers validate --check-repos <instance-root>` before enabling partitioned
claiming, and after changing backlog routing labels. The check is read-only.

`SIB002` names an open `goobers:ready` issue matching none of the configured
label-routing scopes and lists the candidate routes. Each local `backlog-query
--claim` task contributes an alternative scope, using its trust label, required
labels, excluded labels and label predicate. An explicit task `requireLabels`
replaces the gaggle default, including an explicitly empty value. Scopes from
all local gaggles targeting the repository participate.

Declared siblings contribute their `requireLabels` only when their complete
repository identity matches. These declarations are trusted operator metadata,
not observations of the sibling's live configuration. A sibling with no required
labels covers every label combination; stale sibling declarations can therefore
hide gaps. Keep declarations synchronized with the sibling instance.

Correct the issue's partition labels or the configured filters. Do not blindly
apply every suggested label: two alternative routes may deliberately exclude
one another. This check does not change labels, claim work, or grant approval.

The diagnostic measures **label routing**, not full claim eligibility. Assignment,
field predicates, leases, open PRs, workflow readiness and provider permissions
can still prevent a routed item from being claimed. It does not assert that
either instance would actually claim an item.

The live check currently uses the GitHub repository-reality provider path;
other providers retain the explicit not-checked notice. It reads at most ten
100-candidate pages per repository within the existing preflight timeout.
`SIB003` reports an incomplete scan, including a provider failure, invalid
predicate, stalled cursor or reached limit. Findings already observed remain
useful, but absence of findings in an incomplete scan is not an all-clear.
Both codes are advisory and appear in JSON diagnostics without changing the
validation exit code, like the existing repository-reality checks.
