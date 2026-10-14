package capability

import (
	"regexp"
	"strings"
	"time"
)

var nonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]+`)

// CredentialEnvVar returns the deterministic environment variable name for a
// capability-scoped credential.
func CredentialEnvVar(value string) string {
	sanitized := nonAlnum.ReplaceAllString(value, "_")
	return "GOOBERS_CRED_" + strings.ToUpper(sanitized)
}

// CredentialExpiryEnvVar returns the environment variable that carries the
// stated expiry of the credential delivered as CredentialEnvVar(value), as an
// RFC 3339 UTC timestamp (#5905). It is set only when the credential's source
// states an expiry (a Microsoft Entra or GitHub App token; a PAT states
// none). The expiry is not a secret, and the name deliberately does not
// start with "GOOBERS_CRED_": every variable under that prefix is a
// credential value, and code that scans the environment for them treats each
// one as a secret.
func CredentialExpiryEnvVar(value string) string {
	sanitized := nonAlnum.ReplaceAllString(value, "_")
	return "GOOBERS_CREDENTIAL_EXPIRES_" + strings.ToUpper(sanitized)
}

// CredentialSchemeEnvVar returns the environment variable that carries the
// authorization scheme ("basic" or "bearer") of the Azure DevOps credential
// delivered as CredentialEnvVar(value) (#5957). The daemon states it per
// capability because capabilities of one stage can be backed by different
// sources: a credentials: entry's personal access token is sent as Basic even
// when the repository's own Microsoft Entra credential is sent as Bearer. Like
// CredentialExpiryEnvVar the value is not a secret, so the name is outside the
// "GOOBERS_CRED_" prefix whose every variable is treated as one.
func CredentialSchemeEnvVar(value string) string {
	sanitized := nonAlnum.ReplaceAllString(value, "_")
	return "GOOBERS_CREDENTIAL_SCHEME_" + strings.ToUpper(sanitized)
}

// FormatCredentialExpiry renders a stated credential expiry the way
// CredentialExpiryEnvVar carries it.
func FormatCredentialExpiry(expiresAt time.Time) string {
	return expiresAt.UTC().Format(time.RFC3339)
}

// ParseCredentialExpiry reads a value FormatCredentialExpiry wrote. It
// reports false for an empty or malformed value: the expiry is a hint for
// error messages, so a stage never fails on it.
func ParseCredentialExpiry(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	expiresAt, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, false
	}
	return expiresAt, true
}
