package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExemptionPolicyRequiresCurrentOwnerOrExpiry(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	states := map[string]string{"1": "closed", "2": "open"}
	for _, tc := range []struct{ reason, want string }{
		{"Waiting for integration (#2).", ""},
		{"Old and current owners (#1, #2).", ""},
		{"Closed owner (#1).", "only closed"},
		{"Future owner (#999).", "unknown"},
		{"Public API compatibility.", "requires an open"},
		{"Other repository owner/repo#2", "requires an open"},
		{"Owner (#20abc)", "requires an open"},
		{"Time bounded expires=2026-09-07", ""},
		{"Closed owner (#1) expires=2026-09-08", ""},
		{"Open owner (#2) expires=2026-09-06", "expired"},
		{"Open owner (#2) expires=2026-02-30", "invalid expiry"},
		{"expires=", "empty expires"},
		{"expires=2026-09-08 expires=2026-09-09", "multiple expires"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			got := exemptionPolicyProblem(tc.reason, states, now)
			if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("problem=%q want=%q", got, tc.want)
			}
		})
	}
	if got := exemptionPolicyProblem("expires=2026-09-07", states, now.Add(12*time.Hour)); !strings.Contains(got, "expired") {
		t.Fatalf("expiry did not end at next UTC midnight: %q", got)
	}
}

func TestFrozenPolicyBaselineDoesNotAdmitNewOrEditedEntries(t *testing.T) {
	entries, err := parseExemptions(bytes.NewReader(policyBaseline))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	if err := checkExemptionPolicy(entries, "issue-states.json", now, true, &out); err != nil {
		t.Fatalf("rollout must preserve pre-existing ledger: %v", err)
	}
	if !strings.Contains(out.String(), "baseline:") || !strings.Contains(out.String(), "only closed issues") {
		t.Fatalf("legacy debt was not surfaced: %s", out.String())
	}
	const symbol = "github.com/goobers/goobers/example.New"
	entries[symbol] = exemption{reason: "Untracked compatibility seam"}
	if err := checkExemptionPolicy(entries, "issue-states.json", now, false, &out); err == nil || !strings.Contains(err.Error(), symbol) {
		t.Fatalf("new entry escaped policy: %v", err)
	}
	delete(entries, symbol)
	for oldSymbol, entry := range entries {
		entry.reason = "Changed reason without ownership"
		entries[oldSymbol] = entry
		if err := checkExemptionPolicy(entries, "issue-states.json", now, false, &out); err == nil || !strings.Contains(err.Error(), oldSymbol) {
			t.Fatalf("edited entry escaped policy: %v", err)
		}
		break
	}
}

func TestDeadcodeCommandRejectsPolicyBeforeRunningAnalyzer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exemptions.txt")
	if err := os.WriteFile(path, []byte("github.com/goobers/goobers/example.New # Waiting on closed owner (#664).\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	code := run([]string{"-go", "must-not-run", "-exemptions", path, "-issue-states", "issue-states.json"}, &out, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "cites only closed issues") || strings.Contains(stderr.String(), "build analyzer") {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
}

func TestExemptionPolicyRejectsBadStateEvidence(t *testing.T) {
	t.Parallel()
	for _, data := range []string{
		`{`,
		`{"repository":"other/repo","observedAt":"2026-09-07","states":{}}`,
		`{"repository":"Agent-Clubhouse/Goobers","observedAt":"bad","states":{}}`,
		`{"repository":"Agent-Clubhouse/Goobers","observedAt":"2026-09-07","states":{"1":"unknown"}}`,
	} {
		path := filepath.Join(t.TempDir(), "states.json")
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := loadIssueStates(path); err == nil {
			t.Fatalf("accepted invalid state evidence: %s", data)
		}
	}
}
