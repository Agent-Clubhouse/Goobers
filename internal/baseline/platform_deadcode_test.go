package baseline

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/flake"
)

// The fixtures below are the platform-specific dead-code failure #4477 is
// about, in the two shapes the classifier actually sees it (#4434 follow-up).
// `make ci` runs test/deadcode as a prerequisite; the gate analyzes every
// target GOOS from one host and prints one line per unreviewed symbol, with a
// `[platforms: ...]` qualifier when the symbol is dead on only some of them,
// to stderr, then exits 1 under make's trailer.

// deadcodeStageMessage is the executor's message for that failure in the run's
// OWN checkout (captured from internal/executor applyCommandFailureDiagnostic):
// the extracted window spans several lines, and carries the failureDigest
// trailer the probe never sees.
func deadcodeStageMessage(root string) string {
	return "command exited 2; failure: " + root + "/internal/foo/bar_windows.go:12:6: unreviewed dead code: " +
		"github.com/goobers/goobers/internal/foo.helper [platforms: windows]\n" +
		root + "/internal/foo/baz.go:40:6: unreviewed dead code: github.com/goobers/goobers/internal/foo.other\n" +
		"exit status 1\nmake: *** [deadcode] Error 1; 2 distinct failure line(s) recorded in failureDigest"
}

// deadcodeStageText is baselineFailureText's shape: summary, then message.
func deadcodeStageText(root string) string {
	message := deadcodeStageMessage(root)
	return message + "\n" + message
}

// deadcodeProbeTranscript is the SAME failure as the baseline probe sees it:
// the raw combined output of `make ci` in a disposable base checkout, led by
// make's echo of the recipe.
func deadcodeProbeTranscript(root string) string {
	return "go run ./test/deadcode -go go\n" +
		root + "/internal/foo/bar_windows.go:12:6: unreviewed dead code: " +
		"github.com/goobers/goobers/internal/foo.helper [platforms: windows]\n" +
		root + "/internal/foo/baz.go:40:6: unreviewed dead code: github.com/goobers/goobers/internal/foo.other\n" +
		"exit status 1\nmake: *** [deadcode] Error 1\n"
}

// TestInheritedPlatformDeadcodeFailureIsSharedBaseline is #4477's acceptance
// regression: a dead-code failure the pinned base already reproduces — in a
// different checkout directory, so every absolute path differs — must classify
// as a shared baseline failure and park, not be blamed on the branch.
func TestInheritedPlatformDeadcodeFailureIsSharedBaseline(t *testing.T) {
	runSide := FailureSignatureText(deadcodeStageText("/work/runs/run-a/repo"))
	probeSide := FailureSignatureText(deadcodeProbeTranscript("/tmp/baseline-probe-123456/checkout"))
	if flake.NormalizeSignature(runSide) != flake.NormalizeSignature(probeSide) {
		t.Fatalf("run side reduced to %q, probe side to %q; want one signature", runSide, probeSide)
	}

	prober := &stubProber{result: ProbeResult{Output: deadcodeProbeTranscript("/tmp/baseline-probe-123456/checkout")}}
	e := newEvaluator(t, prober)
	decision, err := e.Classify(context.Background(), Request{
		Repo: "acme/web", BaseSHA: "abc123def456", Command: []string{"make", "ci"},
		FailureText: deadcodeStageText("/work/runs/run-a/repo"), RunID: "run-1", Waiter: "101",
	})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if decision.Class != ClassSharedBaselineFailure || !decision.Park {
		t.Fatalf("class = %q park = %v (%s), want a parked %q", decision.Class, decision.Park, decision.Reason, ClassSharedBaselineFailure)
	}
	if !slices.Equal(decision.Platforms, []string{"windows"}) {
		t.Fatalf("platforms = %v, want [windows]: the failure is platform-specific", decision.Platforms)
	}
	if !strings.Contains(decision.Reason, "windows") {
		t.Fatalf("reason = %q, want the platform named for the remediation reader", decision.Reason)
	}
}

// TestBranchAddedDeadcodeIsStillPRIntroduced guards the other direction: a
// branch that adds its OWN dead symbol on top of a base that is red for a
// different one must not hide behind the base.
func TestBranchAddedDeadcodeIsStillPRIntroduced(t *testing.T) {
	const base = "/tmp/baseline-probe-123456/checkout"
	probe := "go run ./test/deadcode -go go\n" +
		base + "/internal/foo/baz.go:40:6: unreviewed dead code: github.com/goobers/goobers/internal/foo.other\n" +
		"exit status 1\nmake: *** [deadcode] Error 1\n"
	e := newEvaluator(t, &stubProber{result: ProbeResult{Output: probe}})
	decision, err := e.Classify(context.Background(), Request{
		Repo: "acme/web", BaseSHA: "abc123def456", Command: []string{"make", "ci"},
		FailureText: deadcodeStageText("/work/runs/run-a/repo"),
	})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if decision.Class != ClassPRIntroduced {
		t.Fatalf("class = %q, want %q: the branch introduced helper's dead code", decision.Class, ClassPRIntroduced)
	}
}

func TestFailureSignatureTextDropsTheDigestAndHintTrailers(t *testing.T) {
	got := FailureSignatureText("command exited 2; failure: a.go:1:2: boom; hint: retry later; 3 distinct failure line(s) recorded in failureDigest")
	if strings.Contains(got, "hint") || strings.Contains(got, "distinct failure") {
		t.Fatalf("FailureSignatureText = %q, want the run-local trailers dropped", got)
	}
}

// deadcodeFindings renders one deadcode finding per name under root, at a
// realistic package depth.
func deadcodeFindings(root string, names ...string) string {
	var b strings.Builder
	for _, name := range names {
		b.WriteString(root + "/internal/runner/recoveryinventory/" + name + "_windows.go:112:6: unreviewed dead code: " +
			"github.com/goobers/goobers/internal/runner/recoveryinventory." + name + " [platforms: windows]\n")
	}
	return b.String()
}

const (
	realisticRunRoot   = "/home/operator/.goobers/instances/main/workcopies/run-9f8e7d6c5b4a/repo"
	realisticProbeRoot = "/var/folders/xy/abcdefgh/T/goobers-baseline-probe-1234567/checkout"
	deadcodeTrailer    = "exit status 1\nmake: *** [Makefile:202: deadcode] Error 1\n"
)

// classifyDeadcode classifies a run whose stage message is exactly what the
// executor derives from runFindings on stderr, against a base whose combined
// transcript carries probeFindings.
func classifyDeadcode(t *testing.T, runFindings, probeFindings string) Decision {
	t.Helper()
	diagnostic := executor.FailureDiagnostic(nil, []byte(runFindings+deadcodeTrailer))
	message := "command exited 2; failure: " + diagnostic + "; 3 distinct failure line(s) recorded in failureDigest"
	probe := "go run ./test/deadcode -go go\n" + probeFindings + deadcodeTrailer
	e := newEvaluator(t, &stubProber{result: ProbeResult{Output: probe}})
	decision, err := e.Classify(context.Background(), Request{
		Repo: "acme/web", BaseSHA: "abc123def456", Command: []string{"make", "ci"},
		FailureText: message + "\n" + message,
	})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	return decision
}

// TestInheritedDeadcodeMatchesAtRealisticPathLengths uses the executor's own
// extraction on the run side and production-length checkout roots.
func TestInheritedDeadcodeMatchesAtRealisticPathLengths(t *testing.T) {
	decision := classifyDeadcode(t,
		deadcodeFindings(realisticRunRoot, "alphaHelper"),
		deadcodeFindings(realisticProbeRoot, "alphaHelper"))
	if decision.Class != ClassSharedBaselineFailure {
		t.Fatalf("class = %q (%s), want %q", decision.Class, decision.Reason, ClassSharedBaselineFailure)
	}
}

// TestBranchFindingBeyondTheVisibleSignatureIsPRIntroduced is the collision
// guard: flake.NormalizeSignature keeps three lines, so a branch adding a
// FOURTH dead symbol to a base already failing three must still be blamed for
// it rather than parked behind the base.
func TestBranchFindingBeyondTheVisibleSignatureIsPRIntroduced(t *testing.T) {
	// Short findings, so the window holds all of them untruncated.
	findings := func(root string, names ...string) string {
		var b strings.Builder
		for _, name := range names {
			b.WriteString(root + "/p/" + name + ".go:1:6: unreviewed dead code: example.com/p." + name + "\n")
		}
		return b.String()
	}
	const runRoot, probeRoot = "/w/run-1/repo", "/tmp/probe-1234567/checkout"
	shared := classifyDeadcode(t,
		findings(runRoot, "a", "b", "c", "d"),
		findings(probeRoot, "a", "b", "c", "d"))
	if shared.Class != ClassSharedBaselineFailure {
		t.Fatalf("identical four-finding failure: class = %q (%s), want %q", shared.Class, shared.Reason, ClassSharedBaselineFailure)
	}
	added := classifyDeadcode(t,
		findings(runRoot, "a", "b", "c", "d"),
		findings(probeRoot, "a", "b", "c"))
	if added.Class != ClassPRIntroduced {
		t.Fatalf("branch-added fourth finding: class = %q, want %q", added.Class, ClassPRIntroduced)
	}
}

// TestTruncatedMultiFindingWindowFailsOpen pins the known limit: the executor
// bounds a failure window at 512 bytes, and whatever lies past the cut is
// unseen — the findings that tell two failures apart may all be there. A
// truncated diagnostic on either side is therefore ClassUnknown (the caller's
// pre-existing routing), never a parked shared failure.
func TestTruncatedMultiFindingWindowFailsOpen(t *testing.T) {
	decision := classifyDeadcode(t,
		deadcodeFindings(realisticRunRoot, "alphaHelper", "betaHelper", "gammaHelper"),
		deadcodeFindings(realisticProbeRoot, "alphaHelper", "betaHelper", "gammaHelper"))
	if decision.Class != ClassUnknown || decision.Park {
		t.Fatalf("class = %q park = %v, want %q for a truncated run diagnostic", decision.Class, decision.Park, ClassUnknown)
	}

	// Run side fits; the base's transcript is long enough to be cut.
	baseOnly := classifyDeadcode(t,
		deadcodeFindings("/w/r", "alphaHelper"),
		deadcodeFindings(realisticProbeRoot, "alphaHelper", "betaHelper", "gammaHelper"))
	if baseOnly.Class == ClassSharedBaselineFailure || baseOnly.Park {
		t.Fatalf("class = %q park = %v, want a truncated baseline never matched", baseOnly.Class, baseOnly.Park)
	}
}

// TestDifferingTruncatedTailsNeverPark guards the wrong-park direction of the
// 512-byte bound: two windows cut at the same line count whose cut-off tails
// differ (the base fails gamma, the branch fixed gamma but added delta; or the
// branch adds a finding the base lacks) must never share a signature. The
// partial last line is evidence, never dropped.
func TestDifferingTruncatedTailsNeverPark(t *testing.T) {
	for pad := 0; pad < 200; pad += 5 {
		runRoot := "/w/run/" + strings.Repeat("r", pad) + "/repo"
		probeRoot := "/w/prb/" + strings.Repeat("p", pad) + "/repo"
		swapped := classifyDeadcode(t,
			deadcodeFindings(runRoot, "alphaHelper", "betaHelper", "deltaHelperX"),
			deadcodeFindings(probeRoot, "alphaHelper", "betaHelper", "gammaHelper"))
		if swapped.Class == ClassSharedBaselineFailure {
			t.Fatalf("pad=%d: branch failing a different third symbol parked, signature %q", pad, swapped.Signature)
		}

		diagnostic := executor.FailureDiagnostic(nil, []byte(deadcodeFindings(runRoot, "alphaHelper", "betaHelper", "gammaHelper")+"exit status 1\n"))
		message := "command exited 2; failure: " + diagnostic
		e := newEvaluator(t, &stubProber{result: ProbeResult{Output: deadcodeFindings(probeRoot, "alphaHelper", "betaHelper") + "exit status 1\n"}})
		added, err := e.Classify(context.Background(), Request{
			Repo: "acme/web", BaseSHA: "abc123def456", Command: []string{"go", "run", "./test/deadcode"},
			FailureText: message + "\n" + message,
		})
		if err != nil {
			t.Fatalf("Classify: %v", err)
		}
		if added.Class == ClassSharedBaselineFailure {
			t.Fatalf("pad=%d: branch-added truncated finding parked, signature %q", pad, added.Signature)
		}
	}
}

// TestEllipsisInAnAssertionIsEvidence: an assertion that merely ends in "..."
// was never truncated and still distinguishes two failures of one test.
func TestEllipsisInAnAssertionIsEvidence(t *testing.T) {
	run := executor.FailureDiagnostic([]byte("--- FAIL: TestX (0.01s)\n    x_test.go:12: waiting for leader...\nFAIL\n"), nil)
	probe := "--- FAIL: TestX (0.02s)\n    x_test.go:40: retrying connection...\nFAIL\n"
	if got, base := failureSignature("command exited 1; failure: "+run), failureSignature(probe); got == base {
		t.Fatalf("different assertions in one test collided on %q", got)
	}
}

func TestFailureSignatureTextKeepsATrailerLookalikeInAnEarlierFinding(t *testing.T) {
	got := FailureSignatureText("command exited 1; failure: a.go:1:2: saw '; hint: x'\nb.go:3:4: boom; 1 distinct failure line(s) recorded in failureDigest")
	if !strings.Contains(got, "b.go:3:4: boom") || strings.Contains(got, "distinct failure") {
		t.Fatalf("FailureSignatureText = %q, want only the final line's trailer dropped", got)
	}
}

func TestFailurePlatforms(t *testing.T) {
	for _, tc := range []struct {
		text string
		want []string
	}{
		{"x.go:1:1: unreviewed dead code: p.f", nil},
		{"x.go:1:1: unreviewed dead code: p.f [platforms: windows]", []string{"windows"}},
		{"a [platforms: linux,darwin]\nb [platforms: windows, darwin]", []string{"darwin", "linux", "windows"}},
	} {
		if got := FailurePlatforms(tc.text); !slices.Equal(got, tc.want) {
			t.Errorf("FailurePlatforms(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}
