package main

import (
	"fmt"
	"os"

	"github.com/goobers/goobers/internal/executor"
)

// commentWatchRoutingBudgetEnabled selects only the versioned budget feature.
// Feedback acknowledgements and explicitly requested landing disarm are shared
// correctness behavior. Stage callers receive the contract from the pinned
// envelope, not GOOBERS_INPUT_* or run.env. A legacy stage without that stamp
// must stop before any provider mutation: guessing modern could post a comment
// its label-only policy never authorized, while guessing 2.0 bypasses the guard.
// Standalone operators without a stage context retain the guarded CLI default.
func commentWatchRoutingBudgetEnabled() (bool, error) {
	switch version := os.Getenv(executor.DSLVersionEnvVar); version {
	case "2.0":
		return false, nil
	case "2.1", "3.0", "3.1":
		return true, nil
	case "":
		if os.Getenv(executor.RunIDEnvVar) == "" && os.Getenv(executor.WorkflowEnvVar) == "" && os.Getenv(executor.TaskEnvVar) == "" {
			return true, nil
		}
		return false, fmt.Errorf("pr-comment-watch: missing pinned DSL version in stage invocation; an upgraded runner must dispatch a new invocation from the pinned workflow definition")
	default:
		return false, fmt.Errorf("pr-comment-watch: unsupported pinned DSL version %q", version)
	}
}
