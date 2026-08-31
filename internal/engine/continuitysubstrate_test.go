package engine

// continuitysubstrate_test.go is the #3767 half of the cross-substrate
// continuity harness (#4009): the declared-edge selector driving REAL
// bundles onto a REAL receiving substrate.
//
// # Why a second file, and why here
//
// continuity_test.go already covers selectDelta exhaustively — but over
// placeholder digests, and its threading assertions run against the engine's
// fakes. That is the right shape for the walk's own logic and the wrong shape
// for the claim #3767 makes, which is not "the selector returns entry X" but
// "the consumer's WORKSPACE ends up carrying X's commits and not Y's". Those
// differ exactly where it matters: a selector that returns the declared entry
// while the substrate lands the last-written one is the silent wrong base the
// issue describes, and no assertion over digest strings can tell them apart.
//
// So this file composes the selector with the production landing path —
// internal/worktree's managed mirror, ApplyBundle's ancestry guard, and a
// real worktree cut on the receiving branch — and asserts on FILES.
//
// The pod↔self direction of the same harness lives in
// cmd/goobers/continuitysubstrate_test.go, where the pod's own adapter is
// reachable. Both write to one evidence schema
// (test/testsupport/continuityevidence) so an acceptance reader gets the two
// halves in the same terms.
//
// # Why every arm carries an ablation
//
// #3767's defect is not a crash; it is a run that looks healthy while
// building on the wrong base. An arm that only shows the correct outcome
// cannot distinguish "the rule chose correctly" from "both rules would have
// chosen the same thing", which is true for every linear chain in the
// workflow tree today and is precisely why the divergence went unnoticed. So
// each arm also lands what the SUPERSEDED rule would have selected and names
// the file that arrives, or is lost, as a result.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/testgit"
	"github.com/goobers/goobers/internal/workspacedelta"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/test/testsupport/continuityevidence"
)

const (
	substrateBase      = "main"
	substrateRunBranch = "goobers/implementation/run-3767"
	substratePRBranch  = "goobers/pr-42"
)

// substrateGit is workspacedelta.Git over the isolated test git.
type substrateGit struct{}

func (substrateGit) Run(_ context.Context, dir string, args ...string) error {
	cmd := testgit.Command(args...)
	cmd.Dir = dir
	return cmd.Run()
}

func (substrateGit) Output(_ context.Context, dir string, args ...string) (string, error) {
	cmd := testgit.Command(args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// substrateFixture is one origin plus the bundles published against it. Each
// call to land() gets its OWN mirror, so an arm that lands a bundle cannot
// contaminate the arm after it — the arms describe alternative worlds (what
// the declared rule does, what the superseded rule would have done), and
// sharing a mirror between them would make the second one's outcome depend
// on the first one's.
type substrateFixture struct {
	t      *testing.T
	origin string
	// bundles is the blob plane, keyed by digest exactly as the store is.
	bundles map[string]workspacedelta.Bundle
	// checkouts is one producer working copy per branch, so a second
	// producer on the same branch builds ON the first.
	checkouts map[string]string
	// root holds the fixture's directories; t.TempDir() returns a NEW
	// directory on every call, so a fixture that used it per publication
	// would silently make each producer start from base again — the exact
	// opposite of the cumulative chain these arms need.
	root string
}

func newSubstrateFixture(t *testing.T) *substrateFixture {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	runSubstrateGit(t, root, "init", "--quiet", "--bare", "-b", substrateBase, origin)
	seed := filepath.Join(root, "seed")
	runSubstrateGit(t, root, "clone", "--quiet", origin, seed)
	writeSubstrateFile(t, seed, "README.md", "base\n")
	runSubstrateGit(t, seed, "add", "README.md")
	commitSubstrate(t, seed, "base")
	runSubstrateGit(t, seed, "push", "--quiet", "origin", substrateBase)
	return &substrateFixture{
		t: t, origin: origin, root: root,
		bundles:   map[string]workspacedelta.Bundle{},
		checkouts: map[string]string{},
	}
}

// publish is one producer stage's publication: a checkout of branch (created
// from base on first use, continued on later use) commits file and publishes
// base..HEAD, returning the continuity entry the walk would have recorded.
//
// The checkout directory is keyed by branch so a second producer on the same
// branch builds ON the first — the cumulative shape a real run has, and the
// one that makes "which entry was selected" observable as file presence.
func (f *substrateFixture) publish(stage, branch, file string) continuityEntry {
	f.t.Helper()
	dir, ok := f.checkouts[branch]
	if !ok {
		dir = filepath.Join(f.root, "producer-"+strings.ReplaceAll(branch, "/", "-"))
		runSubstrateGit(f.t, f.root, "clone", "--quiet", "--branch", substrateBase, f.origin, dir)
		runSubstrateGit(f.t, dir, "checkout", "--quiet", "-B", branch)
		f.checkouts[branch] = dir
	}
	writeSubstrateFile(f.t, dir, file, file+"\n")
	runSubstrateGit(f.t, dir, "add", file)
	commitSubstrate(f.t, dir, "commit "+file)
	bundle, err := workspacedelta.Create(context.Background(), substrateGit{}, dir, "origin/"+substrateBase, "HEAD")
	if err != nil {
		f.t.Fatalf("publish %s on %s: %v", stage, branch, err)
	}
	f.bundles[bundle.Digest] = bundle
	return continuityEntry{Stage: stage, Attempt: 1, Digest: bundle.Digest, Base: bundle.Base, Tip: bundle.Tip, Branch: branch}
}

// land applies entry's real bundle to branch in a FRESH mirror and returns
// the working copy a consuming stage would be handed. This is the production
// path: worktree.Manager.ApplyBundle's ancestry guard, then a worktree cut on
// the receiving branch.
func (f *substrateFixture) land(entry continuityEntry, branch string) string {
	f.t.Helper()
	ctx := context.Background()
	manager, err := worktree.NewManager(filepath.Join(f.t.TempDir(), "mirror"))
	if err != nil {
		f.t.Fatalf("worktree manager: %v", err)
	}
	bundle, ok := f.bundles[entry.Digest]
	if !ok {
		f.t.Fatalf("no bundle published under %s", entry.Digest)
	}
	if _, err := manager.ApplyBundle(ctx, worktree.ApplyBundleOptions{
		RepoURL: f.origin, Branch: branch, BaseRef: substrateBase,
	}, bundle, nil); err != nil {
		f.t.Fatalf("ApplyBundle %s onto %s: %v", entry.Digest, branch, err)
	}
	wt, err := manager.Create(ctx, worktree.CreateOptions{
		RepoURL: f.origin, RunID: "consume-" + entry.Stage, BaseRef: substrateBase, Branch: branch,
	})
	if err != nil {
		f.t.Fatalf("cut consumer worktree on %s: %v", branch, err)
	}
	f.t.Cleanup(func() { _ = wt.Remove(ctx, worktree.RemoveOptions{}) })
	return wt.Path
}

// carries reports whether a landed workspace contains a file.
func carries(t *testing.T, dir, file string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, file))
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	t.Fatalf("stat %s: %v", file, err)
	return false
}

func runSubstrateGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := testgit.Command(args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v (dir=%s): %v: %s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commitSubstrate(t *testing.T, dir, message string) {
	t.Helper()
	runSubstrateGit(t, dir, "-c", "user.name=harness", "-c", "user.email=harness@example.invalid",
		"commit", "--quiet", "-m", message)
}

func writeSubstrateFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestDeclaredRepoHandoffSelectsTheBytesThatLand is the #3767 harness. It
// shares one evidence document with its arms so an acceptance reader gets a
// single artifact per run.
func TestDeclaredRepoHandoffSelectsTheBytesThatLand(t *testing.T) {
	evidence := continuityevidence.New(t, "declared-repo-handoff", "#4009",
		"internal/engine selectDelta (the WF022 runtime rule)",
		"internal/worktree.Manager ApplyBundle + real worktrees",
		"internal/workspacedelta thin bundles over a real git origin",
	)

	t.Run("undeclared producer is refused, and the refusal prevents the wrong bytes landing", func(t *testing.T) {
		refuseUndeclaredProducer(t, evidence)
	})
	t.Run("a stage's own prior attempt is continuity, not a handoff", func(t *testing.T) {
		ownAttemptIsContinuity(t, evidence)
	})
	t.Run("a rebound-branch producer is filtered before the declared arm", func(t *testing.T) {
		reboundProducerIsFiltered(t, evidence)
	})
}

// refuseUndeclaredProducer is #3767's headline shape: the repo chain BRANCHES.
// `implement` and `sidecar` both commit on the run branch; `local-ci` declares
// only `implement`. The superseded last-writer rule hands local-ci sidecar's
// delta, and the run looks fine.
//
// Two things are asserted, and the second is the one that makes this a
// substrate test: the selector refuses, AND the refusal is what stops
// sidecar's file from arriving in the consumer's workspace — demonstrated by
// landing what last-writer would have chosen and naming the file that shows up.
func refuseUndeclaredProducer(t *testing.T, evidence *continuityevidence.Recorder) {
	fixture := newSubstrateFixture(t)
	implement := fixture.publish("implement", substrateRunBranch, "implement.txt")
	sidecar := fixture.publish("sidecar", substrateRunBranch, "sidecar.txt")
	record := []continuityEntry{implement, sidecar}

	_, err := selectDelta(record, "local-ci", []string{"implement"}, substrateRunBranch)
	if err == nil {
		t.Fatal("selectDelta accepted a consumer building on commits its repoFrom does not declare (#3767)")
	}
	for _, want := range []string{`stage "local-ci"`, `commits from "sidecar"`, "repoFrom [implement]", sidecar.Digest} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q: %v", want, err)
		}
	}

	// ABLATION: the rule this replaced. Landing the record's last entry is
	// what #3766 shipped, and it puts a producer the consumer never declared
	// into the consumer's own workspace.
	lastWriter := fixture.land(record[len(record)-1], substrateRunBranch)
	if !carries(t, lastWriter, "sidecar.txt") {
		t.Fatal("the last-writer ablation did not land sidecar's file; it proves nothing about what the refusal prevents")
	}
	if !carries(t, lastWriter, "implement.txt") {
		t.Fatal("the ablation workspace lost implement's file too; the fixture is not producing a cumulative chain")
	}

	// And the declared producer's own bundle carries only what it declared.
	declared := fixture.land(implement, substrateRunBranch)
	if carries(t, declared, "sidecar.txt") {
		t.Fatal("implement's bundle carries sidecar's file; the two producers are not distinguishable")
	}

	evidence.Record(continuityevidence.Assertion{
		Claim: "a 3.0 consumer whose repoFrom does not declare the run's most recent committer " +
			"is refused with a named error instead of silently building on it; the superseded " +
			"last-writer rule lands the undeclared producer's file in the consumer's workspace",
		Kind:      continuityevidence.KindRefusal,
		Direction: continuityevidence.DirectionSelection,
		Refs:      []string{"#3767", "#4009", "#3815"},
		Facts: map[string]string{
			"record":                  "implement -> sidecar (both on " + substrateRunBranch + ")",
			"consumerRepoFrom":        "[implement]",
			"refusal":                 err.Error(),
			"declaredProducerDigest":  implement.Digest,
			"undeclaredLatestDigest":  sidecar.Digest,
			"lastWriterAblationFiles": "implement.txt, sidecar.txt (sidecar undeclared)",
			"declaredArmFiles":        "implement.txt only",
		},
	})
}

// ownAttemptIsContinuity is decision 001 rule 3 at the substrate: a repass
// consumer selects its OWN prior attempt's publication, which is strictly
// newer than the producer it declares.
//
// The ablation here is the other tempting reading of WF022 — "fetch only from
// the declared set" — which would hand the repass the producer's delta and
// silently DISCARD the consumer's own attempt-1 commits. Losing a stage's own
// work is a worse failure than the one #3767 names, so the arm lands both and
// names the file that goes missing.
func ownAttemptIsContinuity(t *testing.T, evidence *continuityevidence.Recorder) {
	fixture := newSubstrateFixture(t)
	implement := fixture.publish("implement", substrateRunBranch, "implement.txt")
	ownAttempt := fixture.publish("local-ci", substrateRunBranch, "ci-fixup.txt")
	record := []continuityEntry{implement, ownAttempt}

	selected, err := selectDelta(record, "local-ci", []string{"implement"}, substrateRunBranch)
	if err != nil {
		t.Fatalf("selectDelta refused a stage's own prior attempt: %v", err)
	}
	if selected.Digest != ownAttempt.Digest {
		t.Fatalf("selectDelta = %+v, want local-ci's own prior publication %s", selected, ownAttempt.Digest)
	}

	continued := fixture.land(selected, substrateRunBranch)
	for _, file := range []string{"implement.txt", "ci-fixup.txt"} {
		if !carries(t, continued, file) {
			t.Fatalf("the repass workspace is missing %s; a repass must continue from its own prior attempt", file)
		}
	}

	// ABLATION: declared-fetch semantics.
	declaredOnly := fixture.land(implement, substrateRunBranch)
	if carries(t, declaredOnly, "ci-fixup.txt") {
		t.Fatal("the declared-fetch ablation still carried the consumer's own work; it proves nothing")
	}

	evidence.Record(continuityevidence.Assertion{
		Claim: "a repassing stage continues from its own prior attempt's publication; the " +
			"declared-fetch reading of WF022 would discard the stage's own commits",
		Kind:      continuityevidence.KindProof,
		Direction: continuityevidence.DirectionSelection,
		Refs:      []string{"#3767", "#4009"},
		Facts: map[string]string{
			"record":                 "implement -> local-ci (own attempt 1)",
			"consumerRepoFrom":       "[implement]",
			"selectedProducer":       selected.Stage,
			"selectedDigest":         selected.Digest,
			"repassWorkspaceFiles":   "implement.txt, ci-fixup.txt",
			"declaredFetchAblation":  "ci-fixup.txt absent (the stage's own work lost)",
			"declaredProducerDigest": implement.Digest,
		},
	})
}

// reboundProducerIsFiltered is the branch key (#392) composed with the
// declared arm: pr-remediation rebinds the run's workspace to a claimed PR's
// head part-way through the lane, so a publication made on the run branch and
// one made on the PR head are not alternatives — they are different lines of
// history.
//
// The branch filter runs FIRST, so a consumer still on the run branch selects
// the run-branch producer even though the PR-head entry is more recent. The
// ablation lands the unfiltered latest entry on the run branch and shows the
// PR's file arriving on a branch it has no business being on.
func reboundProducerIsFiltered(t *testing.T, evidence *continuityevidence.Recorder) {
	fixture := newSubstrateFixture(t)
	implement := fixture.publish("implement", substrateRunBranch, "implement.txt")
	rebased := fixture.publish("rebase-pr", substratePRBranch, "pr-only.txt")
	record := []continuityEntry{implement, rebased}

	selected, err := selectDelta(record, "local-ci", []string{"implement", "rebase-pr"}, substrateRunBranch)
	if err != nil {
		t.Fatalf("selectDelta refused a record whose only undeclared-looking entry is on another branch: %v", err)
	}
	if selected.Digest != implement.Digest {
		t.Fatalf("selectDelta = %+v, want the run-branch producer %s", selected, implement.Digest)
	}

	onBranch := fixture.land(selected, substrateRunBranch)
	if !carries(t, onBranch, "implement.txt") || carries(t, onBranch, "pr-only.txt") {
		t.Fatal("the run-branch consumer's workspace does not match the run-branch producer's publication")
	}

	// ABLATION: no branch key. The unfiltered last entry lands the PR head's
	// commit on the run branch.
	unfiltered := fixture.land(record[len(record)-1], substrateRunBranch)
	if !carries(t, unfiltered, "pr-only.txt") {
		t.Fatal("the unfiltered ablation did not land the rebound branch's file; it proves nothing")
	}

	evidence.Record(continuityevidence.Assertion{
		Claim: "the workspace-branch key filters a rebound-branch publication before the " +
			"declared arm runs, so a consumer on the run branch never receives a PR head's commits",
		Kind:      continuityevidence.KindProof,
		Direction: continuityevidence.DirectionSelection,
		Refs:      []string{"#3767", "#392", "#4009"},
		Facts: map[string]string{
			"record":             "implement@" + substrateRunBranch + " -> rebase-pr@" + substratePRBranch,
			"consumerBranch":     substrateRunBranch,
			"consumerRepoFrom":   "[implement, rebase-pr]",
			"selectedProducer":   selected.Stage,
			"selectedDigest":     selected.Digest,
			"filteredDigest":     rebased.Digest,
			"unfilteredAblation": "pr-only.txt landed on " + substrateRunBranch,
		},
	})
}
