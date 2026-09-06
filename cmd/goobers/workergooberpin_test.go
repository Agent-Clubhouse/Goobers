package main

// The worker's goober-digest pin and refusal (#3884, the half #3912 left
// open). #3912 proved the worker NOTICES a config change; these prove it
// serves each attempt the kit its RUN was admitted against, or says why it
// cannot.
//
// Every test drives the real seams against a real scaffolded instance root and
// real config loading, for the same reason the reload tests do: the seam under
// test is "which config tree did this attempt execute against", and a faked
// tree would fake the answer. The pins are real digests, computed the way the
// daemon computes the one it stamps into run.yaml.
//
// Determinism: forPinnedGaggle and reloadOnce are synchronous and take no
// clock. Only the race test runs goroutines, and it joins all of them.

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/localscheduler"
)

const (
	pinGaggle   = "example"
	pinWorkflow = "default-implement"
	pinGoober   = "coder"
)

// currentPin returns the goober digest the worker's CURRENT tree resolves for
// the demo lane — the value a run admitted against that tree carries.
func currentPin(t *testing.T, seams *workerSeams) string {
	t.Helper()
	snapshot := seams.snapshot.Load()
	if snapshot == nil {
		var err error
		snapshot, _, err = seams.loadConfigSnapshot()
		if err != nil {
			t.Fatalf("load config snapshot: %v", err)
		}
		seams.snapshot.Store(snapshot)
	}
	digest, err := snapshot.gooberDigestFor(pinGaggle, pinWorkflow)
	if err != nil {
		t.Fatalf("goober digest for %s/%s: %v", pinGaggle, pinWorkflow, err)
	}
	return digest
}

// editInstructions rewrites the demo goober's instructions, which is the
// change I-51 was about: a Workflows merge that changes what the curator is
// told to do.
func editInstructions(t *testing.T, root, suffix string) {
	t.Helper()
	path := gooberInstructionsPath(root, pinGaggle, pinGoober)
	writeFileContent(t, path, readFileContent(t, path)+"\n\n"+suffix+"\n")
}

func reloadApplied(t *testing.T, seams *workerSeams) workerReloadOutcome {
	t.Helper()
	outcome, err := seams.reloadOnce()
	if err != nil {
		t.Fatalf("reloadOnce: %v", err)
	}
	if !outcome.Applied {
		t.Fatalf("reload outcome = %+v, want a new config tree applied", outcome)
	}
	return outcome
}

// TestWorkerGooberDigestsAgreeWithTheDaemonForTheSameTree is the foundation
// every other test here stands on, and the one whose failure would be worst:
// the pin a run carries is minted DAEMON-side, and matched WORKER-side. If the
// two compute different digests for identical config, the pin never matches,
// and the loud refusal this issue asks for becomes a permanent outage instead
// of a staleness detector.
//
// It is a real risk, not a hypothetical: the digest is taken over the RESOLVED
// goober specs (harness model and options as admitted), so a worker that
// probed the harness for a model the daemon read from config would digest a
// different spec for the same file. That is why the shared computation is
// factored (compiledMachinesWithGooberDigests) and why both sides defer model
// discovery.
func TestWorkerGooberDigestsAgreeWithTheDaemonForTheSameTree(t *testing.T) {
	root := initDemo(t)
	seams := workerReloadSeams(t, root)
	workerDigest := currentPin(t, seams)

	snapshot := seams.snapshot.Load()
	_, daemonDigests, _, _, err := compiledMachinesWithGooberDigestsAndWarnings(
		instance.NewLayout(root).ConfigDir(),
		snapshot.set,
		goobersByName(snapshot.set),
		snapshot.instructions,
		snapshot.cfg.Runner.EnvPassthrough,
		snapshot.cfg.Runner.HarnessCommand,
		true,
	)
	if err != nil {
		t.Fatalf("daemon-side goober digests: %v", err)
	}
	daemonDigest := daemonDigests[localscheduler.WorkflowIdentity{Gaggle: pinGaggle, Workflow: pinWorkflow}]
	if daemonDigest == "" {
		t.Fatalf("daemon computed no goober digest for %s/%s", pinGaggle, pinWorkflow)
	}
	if workerDigest != daemonDigest {
		t.Fatalf("worker resolves goober digest %s where the daemon mints %s for the same tree; "+
			"every pinned attempt would refuse forever", workerDigest, daemonDigest)
	}
}

// TestWorkerServesAnAttemptWhosePinMatchesTheCurrentTree is the ordinary path:
// the run was admitted against the tree the worker is serving, so the pin is
// satisfied and the attempt gets the same kit an unpinned one would.
func TestWorkerServesAnAttemptWhosePinMatchesTheCurrentTree(t *testing.T) {
	root := initDemo(t)
	seams := workerReloadSeams(t, root)
	pin := currentPin(t, seams)

	pinned, err := seams.forPinnedGaggle(pinGaggle, pinWorkflow, pin)
	if err != nil {
		t.Fatalf("forPinnedGaggle with a matching pin: %v", err)
	}
	unpinned, err := seams.forGaggle(pinGaggle)
	if err != nil {
		t.Fatalf("forGaggle: %v", err)
	}
	if pinned != unpinned {
		t.Fatal("a pin matching the current tree resolved a different kit than the current tree's; " +
			"the pin must select, not duplicate")
	}
}

// TestWorkerServesAnUnpinnedAttemptFromTheCurrentTree is the compatibility
// floor. Runs started before D1 pinned a digest, and every seam that executes
// no goober kit, carry an empty pin — and must keep resolving the current tree
// exactly as they did before this file existed. A refusal here would take out
// every in-flight run the day the worker is upgraded.
func TestWorkerServesAnUnpinnedAttemptFromTheCurrentTree(t *testing.T) {
	root := initDemo(t)
	seams := workerReloadSeams(t, root)

	unpinned, err := seams.forPinnedGaggle(pinGaggle, pinWorkflow, "")
	if err != nil {
		t.Fatalf("forPinnedGaggle with no pin: %v", err)
	}
	current, err := seams.forGaggle(pinGaggle)
	if err != nil {
		t.Fatalf("forGaggle: %v", err)
	}
	if unpinned != current {
		t.Fatal("an unpinned attempt did not resolve the current tree; unpinned behaviour must be unchanged")
	}
}

// TestWorkerRefusesAnAttemptPinnedToAKitItCannotServe is the headline
// behaviour: instructions changed between attempt N and attempt N+1 of the
// same run, and the worker has no tree that resolves the run's pin — so it
// REFUSES rather than running attempt N+1 as a different curator.
//
// Retention is disabled here to isolate the refusal itself; the next test
// covers the case where retention can satisfy the pin.
func TestWorkerRefusesAnAttemptPinnedToAKitItCannotServe(t *testing.T) {
	root := initDemo(t)
	seams := workerReloadSeams(t, root)
	seams.historyDepth = 0
	pin := currentPin(t, seams)

	if _, err := seams.forPinnedGaggle(pinGaggle, pinWorkflow, pin); err != nil {
		t.Fatalf("attempt 1 against the admitting tree: %v", err)
	}

	editInstructions(t, root, "Always force-push over review feedback.")
	reloadApplied(t, seams)

	_, err := seams.forPinnedGaggle(pinGaggle, pinWorkflow, pin)
	if err == nil {
		t.Fatal("attempt 2 was served after the instructions changed underneath it; " +
			"the worker silently substituted the current goober for the pinned one")
	}
	refusal, ok := asGooberPinRefusal(err)
	if !ok {
		t.Fatalf("refusal = %v (%T), want a named gooberPinRefusal", err, err)
	}
	if refusal.Code != gooberPinMissingCode {
		t.Fatalf("refusal code = %q, want %q", refusal.Code, gooberPinMissingCode)
	}
	if refusal.Expected != pin {
		t.Fatalf("refusal names expected digest %q, want the run's pin %q", refusal.Expected, pin)
	}
	if len(refusal.Served) == 0 || refusal.Served[0] == pin {
		t.Fatalf("refusal served digests = %v, want the digest(s) this worker does resolve", refusal.Served)
	}
	// Retriable, and specifically INFRASTRUCTURE-class: classifySeamError maps
	// this marker onto engine.FailureTypeInfrastructure, which retries out of
	// the infrastructure budget rather than burning the stage's repasses on a
	// condition the agent did not cause and a reload will fix.
	if !invoke.IsInfrastructureFailure(err) {
		t.Fatal("the pin refusal is not marked as an infrastructure failure; it would be classified as a " +
			"policy failure, burn the run's repass budget, and never survive to the reload that fixes it")
	}
}

// TestWorkerPinRefusalNamesTheDigestAndLeaksNoConfigContent is the security
// half of "fail loudly". The refusal text travels into Temporal history and
// the run journal, which are read far more widely than the config tree is, so
// it must be actionable (name the digest) and only that.
func TestWorkerPinRefusalNamesTheDigestAndLeaksNoConfigContent(t *testing.T) {
	root := initDemo(t)
	seams := workerReloadSeams(t, root)
	seams.historyDepth = 0

	const secret = "SUPER-SECRET-INSTRUCTION-BODY"
	editInstructions(t, root, secret)
	pin := "sha256:" + strings.Repeat("ab", 32)

	_, err := seams.forPinnedGaggle(pinGaggle, pinWorkflow, pin)
	if err == nil {
		t.Fatal("a pin no tree resolves was served")
	}
	message := err.Error()
	if !strings.Contains(message, pin) {
		t.Fatalf("refusal %q does not name the expected digest %s; an operator cannot act on it", message, pin)
	}
	if !strings.Contains(message, gooberPinMissingCode) {
		t.Fatalf("refusal %q carries no named classification", message)
	}
	if strings.Contains(message, secret) {
		t.Fatalf("refusal leaked instruction content into the run journal: %q", message)
	}
	instructions := readFileContent(t, gooberInstructionsPath(root, pinGaggle, pinGoober))
	for _, line := range strings.Split(instructions, "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 24 {
			continue
		}
		if strings.Contains(message, line) {
			t.Fatalf("refusal quoted a line of the goober's instructions: %q", line)
		}
	}
}

// TestWorkerPreservesInFlightRunIdentityAcrossAReload is the reason snapshots
// are RETAINED rather than dropped, and the concurrent old/new-run case in one
// assertion.
//
// A reload lands between two attempts of run A. Attempt N+1 of A must get A's
// OWN kit — not the new instructions, and not a refusal, because the worker
// still holds the tree A was admitted against. Meanwhile run B, started after
// the reload and pinned to the NEW digest, must get the new kit. Both are in
// flight at once and neither sees the other's tree.
func TestWorkerPreservesInFlightRunIdentityAcrossAReload(t *testing.T) {
	root := initDemo(t)
	seams := workerReloadSeams(t, root)
	oldPin := currentPin(t, seams)

	oldKit, err := seams.forPinnedGaggle(pinGaggle, pinWorkflow, oldPin)
	if err != nil {
		t.Fatalf("run A attempt 1: %v", err)
	}

	editInstructions(t, root, "Prefer squash merges.")
	outcome := reloadApplied(t, seams)
	if len(outcome.RetainedDigests) != 1 {
		t.Fatalf("reload retained %v superseded trees, want exactly the one it replaced", outcome.RetainedDigests)
	}
	newPin := currentPin(t, seams)
	if newPin == oldPin {
		t.Fatal("the instructions edit did not change the lane's goober digest; the fixture proves nothing")
	}

	// Run A, attempt 2: same kit as attempt 1, by pointer. Pointer identity is
	// the strong form of "the run's identity did not move": a rebuilt-but-equal
	// kit would pass a content comparison and still mean the worker went back
	// to disk for it.
	oldAgain, err := seams.forPinnedGaggle(pinGaggle, pinWorkflow, oldPin)
	if err != nil {
		t.Fatalf("run A attempt 2 after the reload: %v", err)
	}
	if oldAgain != oldKit {
		t.Fatal("run A's second attempt got a different kit than its first; " +
			"a reload silently changed an in-flight run's goober")
	}

	// Run B, admitted against the new tree, gets the new tree.
	newKit, err := seams.forPinnedGaggle(pinGaggle, pinWorkflow, newPin)
	if err != nil {
		t.Fatalf("run B pinned to the reloaded tree: %v", err)
	}
	if newKit == oldKit {
		t.Fatal("a run pinned to the NEW digest was served the retained kit; the reload reached no one")
	}
}

// TestWorkerPinRefusalRecoversAfterAReloadBringsThePinnedTree is the far side
// of the refusal, and the operational promise attached to making it retriable:
// the worker whose tree is BEHIND the daemon's (I-51's exact shape) refuses the
// attempt, and the very next attempt after the config sync lands succeeds with
// no restart and no operator action.
func TestWorkerPinRefusalRecoversAfterAReloadBringsThePinnedTree(t *testing.T) {
	root := initDemo(t)
	// The daemon's tree: what the run will be admitted against.
	editInstructions(t, root, "Reviewed by the retargeted owner.")
	daemonSeams := workerReloadSeams(t, root)
	pin := currentPin(t, daemonSeams)

	// The worker's tree, one revision behind. Rolling the file back and
	// starting a fresh seams value is exactly the deploy race: worker pod
	// created before the config tree moved.
	path := gooberInstructionsPath(root, pinGaggle, pinGoober)
	ahead := readFileContent(t, path)
	writeFileContent(t, path, strings.TrimSuffix(ahead, "\n\nReviewed by the retargeted owner.\n"))

	worker := workerReloadSeams(t, root)
	behind := currentPin(t, worker)
	if behind == pin {
		t.Fatal("the stale tree resolves the same digest as the admitting tree; the fixture proves nothing")
	}

	if _, err := worker.forPinnedGaggle(pinGaggle, pinWorkflow, pin); err == nil {
		t.Fatal("the stale worker served an attempt pinned to a tree it does not have — " +
			"this is I-51, with the run believing it ran the new goober")
	} else if refusal, ok := asGooberPinRefusal(err); !ok || refusal.Expected != pin {
		t.Fatalf("stale-tree refusal = %v, want a gooberPinRefusal naming %s", err, pin)
	}

	// The config sync catches up; the worker reloads on its own interval.
	writeFileContent(t, path, ahead)
	reloadApplied(t, worker)

	if _, err := worker.forPinnedGaggle(pinGaggle, pinWorkflow, pin); err != nil {
		t.Fatalf("the retried attempt still refuses after the pinned tree landed: %v; "+
			"a refusal that does not recover on reload is an outage, not a guard", err)
	}
}

// TestWorkerPinRefusalLogsOnceAndClearsOnRecovery is #4153's regression test:
// the previous refusal path never logged anything at all, so a worker whose
// config tree has no writer to ever bring the pinned tree into force (not
// merely one reload behind) retried the exact same refusal forever with no
// operator-visible signal. This proves three things:
//   - the FIRST refusal for a given expected digest is logged loudly, so the
//     retry is now alertable instead of silent;
//   - repeated attempts against the SAME still-missing digest do not spam the
//     log once per retry, mirroring the config watcher's own dedupe;
//   - once the pinned tree lands and the pin resolves, the dedup entry
//     clears, so a LATER divergence against a different digest is loud again
//     rather than assumed already-reported.
func TestWorkerPinRefusalLogsOnceAndClearsOnRecovery(t *testing.T) {
	root := initDemo(t)
	seams := workerReloadSeams(t, root)
	seams.historyDepth = 0

	var logged []string
	seams.logf = func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	pin := "sha256:" + strings.Repeat("ab", 32)

	if _, err := seams.forPinnedGaggle(pinGaggle, pinWorkflow, pin); err == nil {
		t.Fatal("a pin no tree resolves was served")
	}
	if _, err := seams.forPinnedGaggle(pinGaggle, pinWorkflow, pin); err == nil {
		t.Fatal("a pin no tree resolves was served")
	}
	if _, err := seams.forPinnedGaggle(pinGaggle, pinWorkflow, pin); err == nil {
		t.Fatal("a pin no tree resolves was served")
	}

	var refusalLines []string
	for _, line := range logged {
		if strings.Contains(line, gooberPinMissingCode) {
			refusalLines = append(refusalLines, line)
		}
	}
	if len(refusalLines) != 1 {
		t.Fatalf("three identical refusals produced %d logged lines %v, want exactly 1 — "+
			"either the refusal is still silent, or an unbounded retry now spams the log", len(refusalLines), refusalLines)
	}
	if !strings.Contains(refusalLines[0], pin) {
		t.Fatalf("logged refusal %q does not name the expected digest %s", refusalLines[0], pin)
	}

	// The pinned tree lands: recovery, exactly like #3884's own recovery test.
	currentPin(t, seams) // sanity: the current tree resolves something.
	newPin := currentPin(t, seams)
	if _, err := seams.forPinnedGaggle(pinGaggle, pinWorkflow, newPin); err != nil {
		t.Fatalf("attempt against the current tree's own digest: %v", err)
	}

	// A later divergence against a DIFFERENT digest must be loud again, not
	// assumed already-reported because some earlier refusal was logged once.
	logged = nil
	otherPin := "sha256:" + strings.Repeat("cd", 32)
	if _, err := seams.forPinnedGaggle(pinGaggle, pinWorkflow, otherPin); err == nil {
		t.Fatal("a pin no tree resolves was served")
	}
	var secondRefusalLines []string
	for _, line := range logged {
		if strings.Contains(line, gooberPinMissingCode) {
			secondRefusalLines = append(secondRefusalLines, line)
		}
	}
	if len(secondRefusalLines) != 1 {
		t.Fatalf("a new divergence against a different digest produced %d logged lines %v, want exactly 1 — "+
			"recovery must clear the dedup entry rather than silencing every later refusal",
			len(secondRefusalLines), secondRefusalLines)
	}
	if !strings.Contains(secondRefusalLines[0], otherPin) {
		t.Fatalf("logged refusal %q does not name the new expected digest %s", secondRefusalLines[0], otherPin)
	}
}

// TestWorkerRetainedConfigTreesAreBoundedAndEvictOldest pins the bound. Two
// properties, and the second is the one that matters: retention is finite, and
// past the bound the pin fails CLOSED — the worker refuses rather than quietly
// resolving whatever it has left.
func TestWorkerRetainedConfigTreesAreBoundedAndEvictOldest(t *testing.T) {
	root := initDemo(t)
	seams := workerReloadSeams(t, root)
	seams.historyDepth = 2

	oldestPin := currentPin(t, seams)
	if _, err := seams.forPinnedGaggle(pinGaggle, pinWorkflow, oldestPin); err != nil {
		t.Fatalf("attempt against the oldest tree: %v", err)
	}

	pins := []string{oldestPin}
	for i := 0; i < 3; i++ {
		editInstructions(t, root, fmt.Sprintf("Revision %d.", i))
		reloadApplied(t, seams)
		pins = append(pins, currentPin(t, seams))
	}

	if got := len(seams.retainedDigests()); got != seams.historyDepth+1 {
		t.Fatalf("worker holds %d config trees, want %d (current + %d retained)",
			got, seams.historyDepth+1, seams.historyDepth)
	}

	// The three most recent trees (current + 2 retained) still serve.
	for _, pin := range pins[len(pins)-3:] {
		if _, err := seams.forPinnedGaggle(pinGaggle, pinWorkflow, pin); err != nil {
			t.Fatalf("a retained tree's pin %s refused: %v", pin, err)
		}
	}
	// The evicted one does not, and says so by name.
	_, err := seams.forPinnedGaggle(pinGaggle, pinWorkflow, oldestPin)
	if err == nil {
		t.Fatal("an evicted config tree's pin was still served; retention is not bounded")
	}
	if refusal, ok := asGooberPinRefusal(err); !ok || refusal.Code != gooberPinMissingCode {
		t.Fatalf("eviction refusal = %v, want %s", err, gooberPinMissingCode)
	}
}

// TestWorkerRetentionDisabledRefusesEverySupersededPin is the zero-depth
// posture an operator can select, and the one a restart lands in.
func TestWorkerRetentionDisabledRefusesEverySupersededPin(t *testing.T) {
	root := initDemo(t)
	seams := workerReloadSeams(t, root)
	seams.historyDepth = 0

	pin := currentPin(t, seams)
	editInstructions(t, root, "Zero retention.")
	reloadApplied(t, seams)

	if digests := seams.retainedDigests(); len(digests) != 1 {
		t.Fatalf("worker holds %v with retention disabled, want the current tree only", digests)
	}
	if _, err := seams.forPinnedGaggle(pinGaggle, pinWorkflow, pin); err == nil {
		t.Fatal("a superseded pin was served with retention disabled")
	}
}

// TestWorkerRestartHoldsNoHistoryAndRefusesAStalePin is the restart case. A
// worker process holds its retained trees in memory only — a pod restart is
// not a config rollback, and the new process must not pretend it can serve a
// tree it never read. It refuses by name instead, and (as the recovery test
// shows) a run whose pinned tree is the one on disk is served normally.
func TestWorkerRestartHoldsNoHistoryAndRefusesAStalePin(t *testing.T) {
	root := initDemo(t)
	before := workerReloadSeams(t, root)
	stalePin := currentPin(t, before)
	if _, err := before.forPinnedGaggle(pinGaggle, pinWorkflow, stalePin); err != nil {
		t.Fatalf("pre-restart attempt: %v", err)
	}
	editInstructions(t, root, "Landed while the worker was down.")
	reloadApplied(t, before)
	if _, err := before.forPinnedGaggle(pinGaggle, pinWorkflow, stalePin); err != nil {
		t.Fatalf("the pre-restart process should still serve the retained tree: %v", err)
	}

	// The restart: a brand-new seams value over the same instance root.
	after := workerReloadSeams(t, root)
	if digests := after.retainedDigests(); len(digests) != 0 {
		t.Fatalf("a freshly started worker already holds trees %v", digests)
	}
	if _, err := after.forPinnedGaggle(pinGaggle, pinWorkflow, stalePin); err == nil {
		t.Fatal("a restarted worker served a pin for a tree it never read; " +
			"retention must not survive as an assumption")
	} else if _, ok := asGooberPinRefusal(err); !ok {
		t.Fatalf("post-restart refusal = %v, want a named gooberPinRefusal", err)
	}
	// The tree that IS on disk still serves, so a restart is not an outage.
	if _, err := after.forPinnedGaggle(pinGaggle, pinWorkflow, currentPin(t, after)); err != nil {
		t.Fatalf("a restarted worker refused the tree it actually holds: %v", err)
	}
}

// TestWorkerPinResolutionIsRaceSafeAcrossReloads runs the resolver against a
// reloading writer under -race. The invariant asserted is not "no panic" but
// the correctness one: every resolution either returns the kit built from the
// tree that resolves the caller's pin, or refuses — never a kit from a
// different tree.
//
// Verified by construction: each goroutine pins a digest, and the kit it gets
// back must be the one the same pin resolved earlier in the same process.
func TestWorkerPinResolutionIsRaceSafeAcrossReloads(t *testing.T) {
	root := initDemo(t)
	seams := workerReloadSeams(t, root)
	seams.logf = func(string, ...any) {}
	seams.historyDepth = 4

	oldPin := currentPin(t, seams)
	oldKit, err := seams.forPinnedGaggle(pinGaggle, pinWorkflow, oldPin)
	if err != nil {
		t.Fatalf("seed the old kit: %v", err)
	}
	editInstructions(t, root, "Racing revision.")
	reloadApplied(t, seams)
	newPin := currentPin(t, seams)
	newKit, err := seams.forPinnedGaggle(pinGaggle, pinWorkflow, newPin)
	if err != nil {
		t.Fatalf("seed the new kit: %v", err)
	}

	const readers = 4
	const iterations = 60
	var wg sync.WaitGroup
	failures := make(chan string, readers*iterations)

	writerDone := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if _, err := seams.reloadOnce(); err != nil {
				failures <- fmt.Sprintf("reloadOnce: %v", err)
				break
			}
		}
		close(writerDone)
	}()

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			pin, want := oldPin, oldKit
			if r%2 == 1 {
				pin, want = newPin, newKit
			}
			for i := 0; i < iterations; i++ {
				got, err := seams.forPinnedGaggle(pinGaggle, pinWorkflow, pin)
				if err != nil {
					failures <- fmt.Sprintf("pin %s refused mid-race: %v", pin, err)
					return
				}
				if got != want {
					failures <- fmt.Sprintf("pin %s resolved a different kit under concurrency", pin)
					return
				}
			}
		}(r)
	}
	wg.Wait()
	<-writerDone
	close(failures)
	for failure := range failures {
		t.Fatal(failure)
	}
}

// TestWorkerAgenticSeamsResolveTheEnvelopePin is the wiring assertion: the
// envelope field the engine stamps is the one the agentic seam resolves on. It
// is separate from the resolver tests on purpose — every one of those would
// stay green if Invoke/Review quietly went back to forGaggle, which is exactly
// how this regression would return.
func TestWorkerAgenticSeamsResolveTheEnvelopePin(t *testing.T) {
	root := initDemo(t)
	seams := workerReloadSeams(t, root)
	seams.historyDepth = 0
	pin := currentPin(t, seams)

	editInstructions(t, root, "Rolled forward under the run.")
	reloadApplied(t, seams)

	env := apiv1.InvocationEnvelope{
		TaskID:       "run-3884:implement",
		RunID:        "run-3884",
		WorkflowID:   pinWorkflow,
		Gaggle:       pinGaggle,
		Goober:       pinGoober,
		GooberDigest: pin,
	}
	agentic := workerGoober{seams: seams}
	if _, err := agentic.Invoke(t.Context(), env); err == nil {
		t.Fatal("Invoke served an attempt whose pinned tree is gone; the agentic seam ignores the envelope pin")
	} else if _, ok := asGooberPinRefusal(err); !ok {
		t.Fatalf("Invoke error = %v, want the named pin refusal", err)
	}
	if _, err := agentic.Review(t.Context(), env); err == nil {
		t.Fatal("Review served an attempt whose pinned tree is gone; the agentic seam ignores the envelope pin")
	} else if _, ok := asGooberPinRefusal(err); !ok {
		t.Fatalf("Review error = %v, want the named pin refusal", err)
	}

	// And the unpinned envelope still runs the current tree, so the refusal is
	// scoped to pinned attempts rather than to agentic stages in general.
	env.GooberDigest = ""
	if _, err := seams.forPinnedGaggle(env.Gaggle, env.WorkflowID, env.GooberDigest); err != nil {
		t.Fatalf("unpinned envelope refused: %v", err)
	}
}

// TestWorkerKitWriterResolvesThePinnedTree covers the OTHER execution path.
// A mode-3 stage runs in a pod, and its kit is published by agenticKitWriter,
// which used to re-read the config tree from disk on every attempt — a second,
// independent silent-substitution path that no test of the self-execution
// seams would have caught.
func TestWorkerKitWriterResolvesThePinnedTree(t *testing.T) {
	root := initDemo(t)
	seams := workerReloadSeams(t, root)
	pin := currentPin(t, seams)
	if _, err := seams.forPinnedGaggle(pinGaggle, pinWorkflow, pin); err != nil {
		t.Fatalf("seed the admitting tree: %v", err)
	}
	admitted := readFileContent(t, gooberInstructionsPath(root, pinGaggle, pinGoober))

	editInstructions(t, root, "Rolled forward after the run started.")
	reloadApplied(t, seams)

	writer := agenticKitWriter{instanceRoot: root, seams: seams, blobEndpoint: "http://blobs.invalid"}
	env := apiv1.InvocationEnvelope{
		TaskID:       "run-3884:implement",
		RunID:        "run-3884",
		WorkflowID:   pinWorkflow,
		Gaggle:       pinGaggle,
		Goober:       pinGoober,
		GooberDigest: pin,
	}
	kit, err := writer.buildKit(env, "invoke")
	if err != nil {
		t.Fatalf("buildKit against the retained tree: %v", err)
	}
	if got := kit.Instructions[pinGoober]; got != admitted {
		t.Fatalf("the pod's kit carries the rolled-forward instructions, not the ones the run was admitted "+
			"against:\n got %q\nwant %q", got, admitted)
	}

	// A pin no retained tree resolves refuses here too, by the same name, so
	// the pod path cannot become the soft one.
	env.GooberDigest = "sha256:" + strings.Repeat("cd", 32)
	if _, err := writer.buildKit(env, "invoke"); err == nil {
		t.Fatal("the kit writer published a kit for a pin no tree resolves")
	} else if refusal, ok := asGooberPinRefusal(err); !ok || refusal.Expected != env.GooberDigest {
		t.Fatalf("kit writer refusal = %v, want a gooberPinRefusal naming %s", err, env.GooberDigest)
	}

	// A kit writer with no snapshot store must refuse rather than fall back to
	// reading ambient config — the fallback IS the bug.
	if agenticKitWriterFor(root, nil, "http://blobs.invalid", nil) != nil {
		t.Fatal("agenticKitWriterFor built a kit writer with no snapshot store; it would resolve kits from ambient config")
	}
}

// TestWorkerRefusesWhenItCannotComputeAnyDigest is the fail-closed corner: a
// config tree the worker cannot compile into goober digests answers neither
// "yes" nor "no" to a pin, and the distinction matters to whoever reads the
// refusal — "your kit is not here" sends an operator to the config rollout,
// "I cannot tell what is here" sends them to the tree that will not compile.
func TestWorkerRefusesWhenItCannotComputeAnyDigest(t *testing.T) {
	root := initDemo(t)
	seams := workerReloadSeams(t, root)
	pin := currentPin(t, seams)

	// A workflow name the tree does not declare: no identity, so no digest,
	// for any tree this worker holds.
	_, err := seams.forPinnedGaggle(pinGaggle, "no-such-workflow", pin)
	if err == nil {
		t.Fatal("an unknown workflow identity was served")
	}
	refusal, ok := asGooberPinRefusal(err)
	if !ok {
		t.Fatalf("error = %v, want a named gooberPinRefusal", err)
	}
	if refusal.Code != gooberPinUnverifiableCode {
		t.Fatalf("refusal code = %q, want %q for a tree that resolves no digest at all",
			refusal.Code, gooberPinUnverifiableCode)
	}
	if refusal.Detail == "" {
		t.Fatal("an unverifiable refusal carries no detail; the operator cannot tell what could not be evaluated")
	}
	if !invoke.IsInfrastructureFailure(err) {
		t.Fatal("the unverifiable refusal is not retriable")
	}
}

// asGooberPinRefusal reports the refusal carried by err, through wrapping and
// through the infrastructure marker the worker classifies it with. Tests own
// it: production code refuses and lets the classification travel, and never
// needs to unwrap its own refusal back into a struct.
func asGooberPinRefusal(err error) (*gooberPinRefusal, bool) {
	var refusal *gooberPinRefusal
	if errors.As(err, &refusal) {
		return refusal, true
	}
	return nil, false
}

// retainedDigests reports the config-tree digests this worker still holds,
// current first — the observable form of the retention bound.
func (w *workerSeams) retainedDigests() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	if current := w.snapshot.Load(); current != nil {
		out = append(out, current.digest)
	}
	for _, entry := range w.history {
		out = append(out, entry.digest)
	}
	return out
}
