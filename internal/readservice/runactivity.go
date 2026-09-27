package readservice

import "github.com/goobers/goobers/internal/readmodel"

func withRunActivity(summary RunSummary, activity readmodel.StageActivity) RunSummary {
	summary.ActiveStages = activity.Active
	summary.ActivityTruncated = activity.Truncated
	summary.WaitingForGate = activity.WaitingForGate
	return summary
}
