package readservice

func withRunActivity(summary RunSummary, observations runOperationalObservations) RunSummary {
	activity := observations.activity
	summary.reliabilityFacts = observations.reliability
	summary.ActiveStages = activity.Active
	summary.ActivityTruncated = activity.Truncated
	summary.WaitingForGate = activity.WaitingForGate
	return withRunReliability(summary)
}
