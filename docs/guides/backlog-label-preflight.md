# Backlog label preflight

Run `goobers validate --check-repos <instance-root>` to compare configured
backlog workflows with the repository's label definitions. This is read-only;
it neither creates labels nor changes issues.

The GitHub check reports each missing selector, exclusion, claim marker, and
lifecycle label with the repository and referencing workflow/task. It includes
`readyLabel` and `resweepReadyLabel`, their built-in defaults, close-out status
labels, and claims declared through policy actions. Lifecycle labels use the
same derivation as `goobers connect --seed`, so onboarding and validation agree.

A successful label listing can produce missing-label diagnostics (`SELECTOR001`
or `SELECTOR003`). A failed listing produces `REPOLABEL001`: label existence is
unknown, not missing. Resolve repository access or credentials before treating
that repository as checked. The earlier repository reachability preflight may
instead fail with `REPO001` before the label check runs.

Existence comparisons are case-insensitive on GitHub and use the provider's
paginated label listing. Other providers currently report that selector/CI
reality was not checked; an Azure Boards tag is not a GitHub label definition.
The preflight does not claim parity where the provider has no enumeration seam.

Create the reported labels through your repository's normal administration or
onboarding process, then rerun the check. Missing-label warnings are advisory
in ordinary validation and participate in the existing strict validation rules.
