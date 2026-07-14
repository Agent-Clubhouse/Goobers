package telemetry

import (
	"strings"
	"testing"
)

// TestRedactCatchesEachPatternBranch exercises every secretPatterns branch
// directly, rather than only indirectly through the canary tests in
// journalspan_test.go / rollup's aggregate tests. Each case's canary is
// pattern-net-shaped by design — Redact IS the pattern net (this package has
// no registry; see redact.go's doc comment), so a pattern-matching canary is
// the correct, mechanism-isolating input here, unlike a registry-backed
// scrubber where a pattern-matchable canary would give false assurance
// (the #66 finding/standard, docs discussion 2026-07-13).
func TestRedactCatchesEachPatternBranch(t *testing.T) {
	cases := []struct {
		name   string
		secret string
		in     string
	}{
		{"github_ghp", "ghp_0123456789abcdefghijklmnop", "token: ghp_0123456789abcdefghijklmnop"},
		{"github_gho", "gho_0123456789abcdefghijklmnop", "token: gho_0123456789abcdefghijklmnop"},
		{"github_ghu", "ghu_0123456789abcdefghijklmnop", "token: ghu_0123456789abcdefghijklmnop"},
		{"github_ghs", "ghs_0123456789abcdefghijklmnop", "token: ghs_0123456789abcdefghijklmnop"},
		{"github_ghr", "ghr_0123456789abcdefghijklmnop", "token: ghr_0123456789abcdefghijklmnop"},
		{"github_pat", "github_pat_0123456789abcdefghijklmnop", "token: github_pat_0123456789abcdefghijklmnop"},
		{"bearer", "Bearer abc123.def456-ghi789", "Authorization: Bearer abc123.def456-ghi789"},
		{"aws_access_key", "AKIAABCDEFGHIJKLMNOP", "aws key: AKIAABCDEFGHIJKLMNOP"},
		{"pem_private_key", "-----BEGIN PRIVATE KEY-----\nMIIBVgIBADANBgkqhki\n-----END PRIVATE KEY-----",
			"cert:\n-----BEGIN PRIVATE KEY-----\nMIIBVgIBADANBgkqhki\n-----END PRIVATE KEY-----\nend"},
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

// TestRedactDoesNotOverRedact is the companion negative control: ordinary,
// non-secret-shaped text must pass through unchanged. Without this, a
// pattern net tuned to avoid false negatives (missed secrets) could silently
// mangle harmless telemetry content (stage summaries, gate rationale) via
// false positives.
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

// TestRedactDoesNotCoverOpaqueUnshapedSecrets documents, rather than hides,
// this package's real boundary: Redact is a pattern net, the second layer of
// defense-in-depth (redact.go's doc comment) — it has no registry, so an
// opaque, non-standard-shaped issued credential (no recognizable prefix or
// key=value framing) is NOT caught here. That gap is intentionally covered
// by the *primary* net instead: #8's journal scrubber and #14's credential
// registry, which redact by exact registered value regardless of shape. This
// negative control is the standard PM asked audited for post-#66: a
// mechanism-isolating test must show what its mechanism does NOT cover, not
// just what it does.
func TestRedactDoesNotCoverOpaqueUnshapedSecrets(t *testing.T) {
	opaque := "Kf9wQ2mNpZ7-internal-issued-value"
	in := "leaked: " + opaque
	if got := Redact(in); got != in {
		t.Fatalf("Redact(%q) = %q, expected the opaque unshaped value to pass through unchanged (documents the pattern-net's real boundary, not a bug)", in, got)
	}
}
