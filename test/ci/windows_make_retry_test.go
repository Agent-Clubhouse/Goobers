package main

import (
	"strings"
	"testing"
)

// #3139 catalogued every unretried network fetch in ci.yml. The npm/Chromium
// install already got the bounded-retry treatment; this guards the other one
// named in that issue: `choco install make` on the windows-smoke job, which
// reached the Chocolatey feed with a single unretried attempt. The step
// already declares `timeout-minutes: 8`, but that timeout bounds the whole
// step — including the entire 3-attempt loop this test pins — so it does
// not by itself convert a hang into a retry; it only bounds one. This test
// only pins the retry loop's presence, not hang behavior.
func TestWindowsMakeInstallRetriesOnFailure(t *testing.T) {
	w := loadCIWorkflow(t)
	step := w.Jobs["windows-smoke"].step(t, "Install make (windows)")

	// windows-latest defaults to PowerShell, which cannot parse the bash
	// retry loop below.
	if step.Shell != "bash" {
		t.Fatalf("shell = %q, want bash so the retry loop below is parsed correctly", step.Shell)
	}
	if !strings.Contains(step.Run, "for attempt in 1 2 3") {
		t.Fatal("Install make (windows) must retry like the other bounded network fetches in this workflow")
	}
	if !strings.Contains(step.Run, "choco install make") {
		t.Fatal("retry loop must still invoke choco install make")
	}
}
