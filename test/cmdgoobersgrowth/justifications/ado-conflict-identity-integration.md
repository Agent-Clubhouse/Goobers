The fresh-detail conflict recheck must use the existing ADO identity adapter,
retaining case-insensitive UPN matching after selection without changing other
providers. This adds three lines beside the command's workflow eligibility
policy, where the provider-specific comparison belongs. It reuses the adapter
introduced by #7587 and does not add another identity implementation.
