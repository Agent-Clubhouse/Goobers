# Configuring backlog park-label filtering

The shipped backlog-curation workflows exclude parked candidates by default.
Their `query-backlog` task separates park markers from unconditional exclusions:

```yaml
inputs:
  excludeLabels: "goobers:ready"
  parkLabels: "goobers:needs-human,goobers:blocked-on-sibling,goobers:needs-remediation"
  filterParkLabels: "true"
```

Set `filterParkLabels: "false"` to let parked candidates enter ordinary selection.
This only disables the `parkLabels` list. Trust labels, required labels, explicit
`excludeLabels`, CEL predicates, dependency checks, claim ownership, and other
eligibility rules still apply. In particular, this setting does not add
`goobers:ready`, release claims, or clear park markers. Normal curation decisions
and the separately configured bounded re-sweep retain their existing behavior.

Both inputs are task input strings, not instance or gaggle schema fields.
`filterParkLabels` accepts only `"true"` or `"false"` and defaults to `"true"`.
`parkLabels` defaults to an empty list, preserving existing custom workflows.
To migrate an older copied curation workflow, move only its park markers out of
`excludeLabels` into `parkLabels`; labels left in `excludeLabels` always exclude,
even if they also occur in `parkLabels`. Custom park-label names are supported.

For diagnosis, invoke `backlog-query --debug` with the same task inputs supplied
through `GOOBERS_INPUT_PARKLABELS`, `GOOBERS_INPUT_FILTERPARKLABELS`, and the other
`GOOBERS_INPUT_*` variables. Debug output names each excluded candidate's label,
including when every candidate is parked. With filtering disabled, any remaining
exclusion is still reported. A plain query does not claim or relabel candidates.
