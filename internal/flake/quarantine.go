// Package flake provides the repository's explicit, expiring flake quarantine.
package flake

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

const expiryLayout = "2006-01-02"

var issueReference = regexp.MustCompile(`^(?:#[1-9]\d*|https://github\.com/[^/]+/[^/]+/issues/[1-9]\d*)$`)

type testHandle interface {
	Helper()
	Skipf(string, ...any)
	Fatalf(string, ...any)
}

// Quarantine skips a known flake through its expiry date. Invalid or expired
// quarantines fail so every skip remains attached to a live remediation issue.
func Quarantine(t testing.TB, issue, expires string) {
	t.Helper()
	quarantine(t, issue, expires, time.Now())
}

func quarantine(t testHandle, issue, expires string, now time.Time) {
	t.Helper()
	issue = strings.TrimSpace(issue)
	if !issueReference.MatchString(issue) {
		t.Fatalf("flake quarantine requires an issue reference (#123 or a GitHub issue URL), got %q", issue)
		return
	}
	deadline, err := time.Parse(expiryLayout, expires)
	if err != nil || deadline.Format(expiryLayout) != expires {
		t.Fatalf("flake quarantine %s requires an expiry in YYYY-MM-DD format, got %q", issue, expires)
		return
	}
	today, err := time.Parse(expiryLayout, now.UTC().Format(expiryLayout))
	if err != nil {
		t.Fatalf("flake quarantine %s: resolve current UTC date: %v", issue, err)
		return
	}
	if today.After(deadline) {
		t.Fatalf("flake quarantine %s expired on %s; fix the test or renew the issue-backed quarantine", issue, expires)
		return
	}
	t.Skipf("quarantined flake tracked by %s through %s", issue, expires)
}
