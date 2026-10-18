package failureclass

import (
	"regexp"
	"strings"
)

// gitUnresolvableRevisionPattern matches git's own fatal diagnostics for a
// revision argument that does not name anything in the repository: the
// porcelain's "ambiguous argument ... unknown revision" (diff, log, rev-list)
// and the plumbing's "bad revision".
var gitUnresolvableRevisionPattern = regexp.MustCompile(
	`(?i)fatal: (?:ambiguous argument '[^']*': unknown revision or path not in the working tree|bad revision '[^']*')`,
)

// gitFatalExitPattern matches the executor's message prefix for a command that
// exited with git's fatal-usage status.
var gitFatalExitPattern = regexp.MustCompile(`(?i)^command exited 128(?:;|$)`)

// IsGitUnresolvableRevision reports whether one output line is git refusing a
// revision argument it cannot resolve (#5389). The line alone says nothing
// about who wrote the revision; IsUnrunnableGitRevisionFailure adds the exit
// status that makes it a stage-level verdict.
func IsGitUnresolvableRevision(line string) bool {
	return gitUnresolvableRevisionPattern.MatchString(line)
}

// IsUnrunnableGitRevisionFailure reports whether a stage failure message is a
// git command that exited 128 because a revision it was given does not exist
// in the checkout — e.g. a stage command naming `origin/main` in a workcopy
// whose base branch is `refs/heads/main`. Such a command fails identically for
// every diff, so it is a configuration fault rather than evidence about the
// work. Requiring git's own exit status keeps a wrapper (make, a test runner)
// that merely printed the line and exited with its own code classified as a
// work failure.
func IsUnrunnableGitRevisionFailure(message string) bool {
	message = strings.TrimSpace(message)
	return gitFatalExitPattern.MatchString(message) && IsGitUnresolvableRevision(message)
}
