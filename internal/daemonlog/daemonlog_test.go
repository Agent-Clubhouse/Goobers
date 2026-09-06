package daemonlog

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestMergedWriterPreservesInterleavingAndTimestamps(t *testing.T) {
	var out bytes.Buffer
	fixed := time.Date(2026, 9, 5, 13, 20, 45, 0, time.UTC)
	w := NewMergedWriter(&out)
	w.now = func() time.Time { return fixed }

	// Two "streams" (stdout, stderr) sharing one MergedWriter, interleaved as
	// they'd arrive from concurrent writes on the same underlying fd pattern
	// exec.Cmd uses when both Stdout and Stderr point at the same io.Writer.
	if _, err := w.Write([]byte("stdout line 1\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("stderr line 1\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("stdout line 2 (no newline yet")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	wantOrder := []string{"stdout line 1", "stderr line 1", "stdout line 2"}
	lastIdx := -1
	for _, want := range wantOrder {
		idx := strings.Index(got, want)
		if idx < 0 {
			t.Fatalf("output %q missing line %q", got, want)
		}
		if idx < lastIdx {
			t.Fatalf("output %q did not preserve interleaving order", got)
		}
		lastIdx = idx
	}
	stamp := fixed.Format(time.RFC3339Nano)
	if count := strings.Count(got, stamp); count != len(wantOrder) {
		t.Fatalf("output %q: timestamp %q appeared %d times, want %d", got, stamp, count, len(wantOrder))
	}
}

func TestRedactStripsCredentialsAndTokens(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "basic auth url",
			in:   "clone failed: https://user:hunter2@example.com/repo.git unreachable",
			want: "https://[redacted]@example.com/repo.git",
		},
		{
			name: "authorization header",
			in:   "request failed: Authorization: Bearer abc123.def456 (401)",
			want: "Authorization: [redacted]",
		},
		{
			name: "bare bearer token",
			in:   `sent header "bearer sk-live-abcdef" to webhook endpoint`,
			want: "bearer [redacted]",
		},
		{
			name: "token query param",
			in:   "GET /callback?token=abcdef123456&other=1",
			want: "token=[redacted]",
		},
		{
			name: "env-style secret",
			in:   "using GITHUB_TOKEN api_key=sk-abc123",
			want: "api_key=[redacted]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Redact(tc.in)
			if strings.Contains(got, "hunter2") || strings.Contains(got, "abc123.def456") ||
				strings.Contains(got, "sk-live-abcdef") || strings.Contains(got, "abcdef123456") ||
				strings.Contains(got, "sk-abc123") {
				t.Fatalf("Redact(%q) = %q, secret leaked", tc.in, got)
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("Redact(%q) = %q, want substring %q", tc.in, got, tc.want)
			}
		})
	}
}
