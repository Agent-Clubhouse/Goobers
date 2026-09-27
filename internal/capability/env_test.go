package capability

import (
	"strings"
	"testing"
	"time"
)

// TestCredentialExpiryEnvVarIsOutsideTheCredentialPrefix pins #5905's
// naming: the expiry variable sits beside the credential it describes but
// never under "GOOBERS_CRED_", whose values are treated as secrets.
func TestCredentialExpiryEnvVarIsOutsideTheCredentialPrefix(t *testing.T) {
	got := CredentialExpiryEnvVar("github:pr:write")
	if got != "GOOBERS_CREDENTIAL_EXPIRES_GITHUB_PR_WRITE" {
		t.Fatalf("CredentialExpiryEnvVar = %q", got)
	}
	if strings.HasPrefix(got, "GOOBERS_CRED_") {
		t.Fatalf("%q is under the credential-value prefix", got)
	}
}

func TestCredentialExpiryRoundTrips(t *testing.T) {
	expiresAt := time.Date(2026, 9, 28, 13, 4, 5, 0, time.FixedZone("x", 3600))
	parsed, ok := ParseCredentialExpiry(FormatCredentialExpiry(expiresAt))
	if !ok || !parsed.Equal(expiresAt) {
		t.Fatalf("round trip = %v, %v; want %v", parsed, ok, expiresAt)
	}
	for _, bad := range []string{"", "  ", "not-a-time", "1759064645"} {
		if _, ok := ParseCredentialExpiry(bad); ok {
			t.Fatalf("ParseCredentialExpiry(%q) accepted", bad)
		}
	}
}
