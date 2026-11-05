The optimized ADO selection snapshot must use the same UPN identity matcher as
the existing ADO resolver. Three callback lines connect the snapshot's common
resolver to that adapter, while partial scans and the cheap eligibility filter
reuse it directly. This command-local integration preserves the existing
selection policy without adding another provider operation or matcher.
