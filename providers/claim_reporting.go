package providers

func claimFailureRef(provider ProviderKind, req ClaimWorkItemRequest, operation string) ExternalRef {
	return ExternalRef{
		Provider: provider, Ref: issueRef(req.Repository, req.ID),
		Operation: operation, RunID: req.RunID, Outcome: "failure", ErrorCode: "provider_claim_failed",
	}
}

func claimAttemptOutcome(claimed bool) string {
	if claimed {
		return "success"
	}
	return "conflict"
}
