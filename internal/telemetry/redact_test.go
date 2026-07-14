package telemetry

import (
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

// TestRedactCatchesEachPatternBranch exercises every branch of the redaction net
// directly. The provider-token cases use realistic token lengths (a real ghp_ is
// ghp_ + 36 chars) so each exercises journal's canonical net — the single source
// this package now shares (#117) — rather than only the telemetry-only key=value
// net. Each canary is pattern-net-shaped by design: Redact IS the pattern net
// (this package holds no registry; see redact.go), so a pattern-matching canary
// is the correct, mechanism-isolating input here.
func TestRedactCatchesEachPatternBranch(t *testing.T) {
	cases := []struct {
		name   string
		secret string
		in     string
	}{
		// Provider-token net (shared from journal): realistic lengths.
		{"github_ghp", "ghp_abcdefghijklmnopqrstuvwxyz0123456789", "logged ghp_abcdefghijklmnopqrstuvwxyz0123456789 here"},
		{"github_gho", "gho_abcdefghijklmnopqrstuvwxyz0123456789", "logged gho_abcdefghijklmnopqrstuvwxyz0123456789 here"},
		{"github_ghu", "ghu_abcdefghijklmnopqrstuvwxyz0123456789", "logged ghu_abcdefghijklmnopqrstuvwxyz0123456789 here"},
		{"github_ghs", "ghs_abcdefghijklmnopqrstuvwxyz0123456789", "logged ghs_abcdefghijklmnopqrstuvwxyz0123456789 here"},
		{"github_ghr", "ghr_abcdefghijklmnopqrstuvwxyz0123456789", "logged ghr_abcdefghijklmnopqrstuvwxyz0123456789 here"},
		{"github_pat", "github_pat_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMN",
			"logged github_pat_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMN here"},
		{"slack", "xoxb-abcdefghij0123456789", "slack call xoxb-abcdefghij0123456789 done"}, // journal-only pre-#117
		{"aws_access_key", "AKIAABCDEFGHIJKLMNOP", "aws key AKIAABCDEFGHIJKLMNOP set"},
		{"bearer", "Bearer abc123.def456-ghi789", "Authorization: Bearer abc123.def456-ghi789"},
		{"pem_private_key", "-----BEGIN PRIVATE KEY-----\nMIIBVgIBADANBgkqhki\n-----END PRIVATE KEY-----",
			"cert:\n-----BEGIN PRIVATE KEY-----\nMIIBVgIBADANBgkqhki\n-----END PRIVATE KEY-----\nend"},
		// Telemetry-only key=value net (ephemeral output; not in journal's at-rest net).
		{"kv_token", `token="sk_live_abcdefgh12345678"`, `config: token="sk_live_abcdefgh12345678"`},
		{"kv_api_key", "api_key: abcdefgh12345678", "env: api_key: abcdefgh12345678"},
		{"kv_secret", "secret=abcdefgh12345678", "env: secret=abcdefgh12345678"},
		{"kv_password", `password: "abcdefgh12345678"`, `login: password: "abcdefgh12345678"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Redact(tc.in)
			if strings.Contains(got, tc.secret) {
				t.Fatalf("Redact(%q) = %q, still contains the secret", tc.in, got)
			}
			if !strings.Contains(got, RedactedPlaceholder) {
				t.Fatalf("Redact(%q) = %q, expected the redaction placeholder present", tc.in, got)
			}
		})
	}
}

// TestRedactSharesJournalProviderNet is the #117 negative control for pattern-net
// drift: the provider-token net is journal's canonical net, not a divergent copy,
// so telemetry redaction of provider tokens is byte-identical to journal's. The
// Slack token is the proving canary — this package's pre-#117 net had no Slack
// pattern, so it would have left it at rest in a span while the journal redacted
// it. Sharing the net closes that gap and keeps the two in lockstep.
func TestRedactSharesJournalProviderNet(t *testing.T) {
	const slack = "xoxb-abcdefghij0123456789"
	in := "slack webhook " + slack + " fired"

	got := Redact(in)
	if strings.Contains(got, slack) {
		t.Fatalf("Slack token not redacted (pre-#117 telemetry net missed it): %q", got)
	}
	// Byte-identical to journal's net for this provider-token input (no key=value
	// framing here), proving a single shared source rather than two lists.
	if want := string(journal.NewPatternScrubber().Scrub([]byte(in))); got != want {
		t.Fatalf("telemetry net diverged from journal: Redact=%q journal=%q", got, want)
	}
	if RedactedPlaceholder != journal.Redacted {
		t.Fatalf("placeholder drift: telemetry %q vs journal %q", RedactedPlaceholder, journal.Redacted)
	}
}

// TestRedactDoesNotOverRedact is the companion negative control: ordinary,
// non-secret-shaped text must pass through unchanged. Without this, a pattern net
// tuned to avoid false negatives could silently mangle harmless telemetry content
// (stage summaries, gate rationale) via false positives.
func TestRedactDoesNotOverRedact(t *testing.T) {
	cases := []string{
		"the build stage completed successfully",
		"gate review: verdict=approve, target=deploy",
		"issue #42 claimed by run abc123",
		"retrying after timeout, attempt 2 of 3",
	}
	for _, s := range cases {
		if got := Redact(s); got != s {
			t.Fatalf("Redact(%q) = %q, expected unchanged (no secret-shaped content)", s, got)
		}
	}
}

// TestRedactDoesNotCoverOpaqueUnshapedSecrets documents, rather than hides, this
// package's real boundary: Redact is a pattern net, the second layer of
// defense-in-depth — it has no registry, so an opaque, non-standard-shaped issued
// credential (no recognizable prefix or key=value framing) is NOT caught here.
// That gap is intentionally covered by the primary net instead: #8's journal
// scrubber and #14's credential registry, which redact by exact registered value
// regardless of shape.
func TestRedactDoesNotCoverOpaqueUnshapedSecrets(t *testing.T) {
	opaque := "Kf9wQ2mNpZ7-internal-issued-value"
	in := "leaked: " + opaque
	if got := Redact(in); got != in {
		t.Fatalf("Redact(%q) = %q, expected the opaque unshaped value to pass through unchanged", in, got)
	}
}
