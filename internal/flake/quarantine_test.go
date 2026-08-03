package flake

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

type recordingTestHandle struct {
	fatal string
	skip  string
}

func (r *recordingTestHandle) Helper() {}

func (r *recordingTestHandle) Skipf(format string, args ...any) {
	r.skip = fmt.Sprintf(format, args...)
}

func (r *recordingTestHandle) Fatalf(format string, args ...any) {
	r.fatal = fmt.Sprintf(format, args...)
}

func TestQuarantineSkipsThroughExpiryDate(t *testing.T) {
	t.Parallel()
	for _, issue := range []string{"#664", "https://github.com/Agent-Clubhouse/Goobers/issues/664"} {
		handle := &recordingTestHandle{}
		quarantine(handle, issue, "2026-08-15", time.Date(2026, 8, 15, 23, 59, 0, 0, time.UTC))
		if handle.fatal != "" || !strings.Contains(handle.skip, issue) {
			t.Fatalf("issue %q: fatal=%q skip=%q", issue, handle.fatal, handle.skip)
		}
	}
}

func TestQuarantineFailsAfterExpiry(t *testing.T) {
	t.Parallel()
	handle := &recordingTestHandle{}
	quarantine(handle, "#664", "2026-08-15", time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC))
	if !strings.Contains(handle.fatal, "expired on 2026-08-15") || handle.skip != "" {
		t.Fatalf("fatal=%q skip=%q", handle.fatal, handle.skip)
	}
}

func TestQuarantineRejectsAnonymousOrMalformedEntries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		issue   string
		expires string
		want    string
	}{
		{name: "missing issue", expires: "2026-08-15", want: "issue reference"},
		{name: "plain number", issue: "664", expires: "2026-08-15", want: "issue reference"},
		{name: "missing expiry", issue: "#664", want: "YYYY-MM-DD"},
		{name: "invalid expiry", issue: "#664", expires: "2026-02-30", want: "YYYY-MM-DD"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			handle := &recordingTestHandle{}
			quarantine(handle, test.issue, test.expires, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
			if !strings.Contains(handle.fatal, test.want) || handle.skip != "" {
				t.Fatalf("fatal=%q skip=%q, want fatal containing %q", handle.fatal, handle.skip, test.want)
			}
		})
	}
}
