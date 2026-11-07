package readservice

func withRunActivity(summary RunSummary, observations runOperationalObservations) RunSummary {
	activity := observations.activity
	allowances := summary.reliabilityFacts.Allowances
	summary.reliabilityFacts = observations.reliability
	summary.reliabilityFacts.Allowances = allowances
	summary.ActiveStages = activity.Active
	summary.ActivityTruncated = activity.Truncated
	summary.WaitingForGate = activity.WaitingForGate
	summary.TerminalSummary = observations.terminal.Summary(summary.Phase, observations.reliability.TerminalCause)
	return withRunReliability(summary)
}
