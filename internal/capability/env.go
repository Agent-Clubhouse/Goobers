package capability

import (
	"regexp"
	"strings"
)

var nonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]+`)

// CredentialEnvVar returns the deterministic environment variable name for a
// capability-scoped credential.
func CredentialEnvVar(value string) string {
	sanitized := nonAlnum.ReplaceAllString(value, "_")
	return "GOOBERS_CRED_" + strings.ToUpper(sanitized)
}
