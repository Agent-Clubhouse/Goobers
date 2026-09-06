package main

import (
	"os"
	"strings"
	"testing"
)

// TestSweepOrphanedEphemeralTmpRunsBeforeResume guards the exact ordering bug
// caught in review of #4420: resuming an interrupted run dispatches its next
// stage in a background goroutine (tracked by a WaitGroup joined much later,
// near shutdown), and a resumed run's stage can call ephemeraltmp.Establish
// to create a brand-new, live goobers-ephemeral-tmp-* directory.
// SweepOrphans cannot distinguish that live directory from a genuine
// prior-generation orphan by inspecting the filesystem alone — the only
// thing that makes the sweep safe is that this process has not yet
// established a Scope of its own, which is only guaranteed if the sweep
// runs strictly before crash-resume starts spawning those goroutines.
//
// A timing-based test cannot reliably prove a race's absence — the resumed
// goroutine either wins or loses the race depending on scheduling, so a test
// that races them directly could pass even with the bug reintroduced. This
// instead asserts the one thing that actually guarantees safety: the sweep
// call's source position precedes the resume call's, in program order,
// inside runUpContextWithForce.
func TestSweepOrphanedEphemeralTmpRunsBeforeResume(t *testing.T) {
	source, err := os.ReadFile("up.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	const sweepCall = "sweepOrphanedEphemeralTmp(setup.Config, setup.InstanceLog)"
	const resumeCall = "resumeInterruptedRunsWithRunners("
	sweepAt := strings.Index(text, sweepCall)
	resumeAt := strings.Index(text, resumeCall)
	if sweepAt < 0 {
		t.Fatalf("call to %s not found in up.go", sweepCall)
	}
	if resumeAt < 0 {
		t.Fatalf("call to %s not found in up.go", resumeCall)
	}
	if sweepAt > resumeAt {
		t.Fatalf("sweepOrphanedEphemeralTmp must be called before resumeInterruptedRunsWithRunners "+
			"(found sweep at byte %d, resume at byte %d): resume dispatches resumed runs' next stage "+
			"in background goroutines not joined until shutdown, so a sweep running after resume starts "+
			"can delete a resumed run's brand-new, live temp directory instead of only genuine orphans",
			sweepAt, resumeAt)
	}
}
