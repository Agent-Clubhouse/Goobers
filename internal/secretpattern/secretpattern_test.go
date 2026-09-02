package secretpattern

import (
	"strings"
	"testing"
)

func TestScrubRedactsProviderTokenShapes(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"github classic", "token=ghp_" + strings.Repeat("a", 36)},
		{"github fine grained", "token=github_pat_" + strings.Repeat("b", 50)},
		{"aws access key", "id=AKIA" + strings.Repeat("C", 16)},
		{"slack", "token=xoxb-" + strings.Repeat("d", 20)},
		{"pem", "-----BEGIN RSA PRIVATE KEY-----\nzzzz\n-----END RSA PRIVATE KEY-----"},
	}
	s := NewScrubber()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(s.Scrub([]byte(tc.in)))
			if !strings.Contains(got, Redacted) {
				t.Fatalf("value was not redacted")
			}
		})
	}
}

// The authorization patterns redact the credential but keep the scheme, so
// scrubbed evidence still reads as a well-formed header (#3135).
func TestScrubKeepsAuthorizationSchemeAndDropsTheValue(t *testing.T) {
	s := NewScrubber()
	for _, in := range []string{
		"Authorization: Bearer " + strings.Repeat("e", 24),
		"authorization: Basic " + strings.Repeat("f", 24),
	} {
		got := string(s.Scrub([]byte(in)))
		if !strings.Contains(got, RedactedToken) {
			t.Fatalf("credential was not redacted")
		}
		scheme := strings.SplitN(in, " ", 3)[1]
		if !strings.Contains(got, scheme) {
			t.Fatalf("scheme %q did not survive scrubbing: %q", scheme, got)
		}
	}
}

func TestScrubLeavesOrdinaryTextUnchanged(t *testing.T) {
	in := "repo=Agent-Clubhouse/Goobers issue=2931 branch=main"
	if got := string(NewScrubber().Scrub([]byte(in))); got != in {
		t.Fatalf("ordinary text was modified: %q", got)
	}
}
