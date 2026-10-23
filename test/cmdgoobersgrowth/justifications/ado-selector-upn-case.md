# ADO selector identity comparison

The ADO PR selection adapter must retain case-insensitive author and reviewer
matching when the shared selection loop filters scheduled and webhook-targeted
candidates. This adds one small adapter file and an optional summary matcher to
the existing selection resolver. The provider-neutral title/assignee predicates
and other providers' identity matching remain unchanged.

This logic belongs beside the command's private selection resolver: it adapts
its provider-specific candidate selection, rather than defining a new provider
operation. Moving the resolver or adding a public provider API solely for this
callback would broaden this fix. Regression tests cover both selection paths,
mismatches, title predicates, and the unchanged default matcher.
