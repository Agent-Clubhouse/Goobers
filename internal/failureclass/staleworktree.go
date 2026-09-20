package failureclass

import (
	"regexp"
	"strings"
)

var managedWorktreePathPattern = regexp.MustCompile(`(?i)(?:^|[\\/])wt-[0-9a-f]{24}(?:[\\/]|$)`)

var missingPathMarkers = []string{
	"the system cannot find the path specified",
	"no such file or directory",
}

// IsStaleManagedWorktreePath reports whether a diagnostic references a
// missing checkout whose directory has Goobers' managed worktree shape.
// Requiring both the generated path segment and an OS missing-path message
// keeps ordinary missing source files classified as implementation failures.
func IsStaleManagedWorktreePath(message string) bool {
	message = strings.ToLower(message)
	return managedWorktreePathPattern.MatchString(message) && containsAny(message, missingPathMarkers)
}
