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
// bounds a failure window at 512 bytes, so several long findings are cut at a
// point that depends on each checkout's path length and recipe echo. The two
// windows then hold different complete findings and the comparison must fail
// OPEN, to the pre-existing branch attribution, never park.
func TestTruncatedMultiFindingWindowFailsOpen(t *testing.T) {
	decision := classifyDeadcode(t,
		deadcodeFindings(realisticRunRoot, "alphaHelper", "betaHelper", "gammaHelper"),
		deadcodeFindings(realisticProbeRoot, "alphaHelper", "betaHelper", "gammaHelper"))
	if decision.Class == ClassSharedBaselineFailure || decision.Park {
		t.Fatalf("class = %q park = %v, want a truncated window never parked", decision.Class, decision.Park)
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
