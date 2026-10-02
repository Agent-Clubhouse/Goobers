package journal

import "strings"

// StageArtifactName is the SUBSTRATE-INDEPENDENT spelling of a stage
// artifact's journal name: "<stage>/<name>", with the run qualifier the local
// executor carries stripped off.
//
// #4119: the two arms do not agree on that qualifier. internal/executor names
// every stage artifact after env.TaskID, which internal/runner builds as
// `in.RunID + ":" + stageName`, so on a self runner an artifact is recorded as
// "<runID>:<stage>/<name>". A pod names the same artifact "<stage>/<name>",
// because the engine strips the run prefix before stamping GOOBERS_STAGE and
// cmd/goobers/dispatchexec.go builds the name from that. Both spellings are
// legitimate and neither is going away, so a reader that hard-codes either one
// is blind on the other substrate — measured live: a successful
// gather-pr-context whose brief the next stage reported did not exist.
//
// Callers compare against "<stage>/<name>" and are correct on both.
func StageArtifactName(runID, recorded string) string {
	if runID == "" {
		return recorded
	}
	return strings.TrimPrefix(recorded, runID+":")
}
