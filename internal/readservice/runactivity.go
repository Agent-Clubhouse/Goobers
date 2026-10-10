package readservice

func withRunActivity(summary RunSummary, observations runOperationalObservations) RunSummary {
	activity := observations.activity
	allowances := summary.reliabilityFacts.Allowances
	summary.reliabilityFacts = observations.reliability
	summary.reliabilityFacts.Allowances = allowances
	summary.ActiveStages = activity.Active
	summary.ActivityTruncated = activity.Truncated
	summary.WaitingForGate = activity.WaitingForGate
	return withRunReliability(summary)
}
