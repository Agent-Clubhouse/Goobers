package main

// workergooberpin.go is the pin/refusal half of #3884.
//
// #3912 gave `goobers worker` a config-tree reload, which removed the STALE
// WINDOW that cost Infra LEDGER I-51 a run's worktree checkout. It did not
// make kit identity per-run: a reload landing between attempt N and attempt
// N+1 of the same run could still hand N+1 a different curator than N, with
// nothing in run.yaml to show it. #3876/D1 then pinned the run's
// `GooberDigest` into engine.StartSpec / RunInput / run.yaml — provenance
// only, explicitly not selection.
//
// This file closes the loop. The pin rides every InvocationEnvelope, and an
// agentic attempt is served from the config snapshot whose tree RESOLVES that
// digest — the current one, or a bounded history of superseded ones — or it is
// refused by name. There is no third branch: the worker never substitutes its
// current instructions for the ones a run was admitted against.
//
// THE THREE OUTCOMES, and why each is the one it is:
//
//   - The current tree resolves the pin. Ordinary. Serve it.
//   - A retained tree resolves the pin. A reload rolled the tree forward
//     under an in-flight run. Serve that run its OWN kit, so its identity is
//     stable across the reload, while every newly-started run gets the new
//     tree. This is the whole reason snapshots are retained rather than
//     dropped.
//   - Nothing resolves the pin. REFUSE, retriably, naming the expected digest.
//     Retriably because the fix is usually a reload away — the worker's tree
//     is behind the daemon's, the very I-51 shape — and the next attempt after
//     it lands succeeds without operator action. Naming the digest because a
//     refusal an operator cannot act on is only marginally better than the
//     silent stale run it replaced.
//
// Nothing here changes an UNPINNED attempt: an empty envelope digest resolves
// the current tree exactly as before, byte for byte, which is what keeps every
// run started before D1 (and every non-agentic seam) working unchanged.

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/workflow"
)

// workerConfigHistoryDepth is how many superseded config snapshots a worker
// retains so a run pinned to one of them can still be served its own kit
// across a reload.
//
// WHY BOUNDED, AND WHY THIS SMALL.
//
// The window that has to be covered is "a reload landed between two attempts
// of one in-flight run". Config trees move on deploy cadence (minutes to
// days); attempts within a run are seconds to tens of minutes apart. Three
// superseded trees therefore covers the realistic case with room to spare,
// while keeping the retained set small enough that it is obviously bounded:
// each entry holds a parsed config set, the instruction bodies and skill files
// captured with it, and any kits already built from it (credential resolvers,
// worktree managers, harness preflight results).
//
// Unbounded retention is not the safe direction here. The worker is the
// process that must not grow without limit, and a tree no live run is pinned
// to is retained authority for instructions nobody is entitled to run. Past
// the bound the pin fails CLOSED — a named, retriable refusal — which is the
// posture this whole issue exists to install.
const workerConfigHistoryDepth = 3

// Refusal codes, deliberately the credential plane's vocabulary
// (credentialplane.go's stageProfile refusals) rather than a private one: an
// operator who has seen `gate_pin_missing` from the credential plane is
// looking at the same class of fact here — "this run pinned something, and the
// thing serving it cannot honour the pin" — and two names for one class is how
// a runbook goes stale.
const (
	// gooberPinMissingCode names a pin no tree this worker holds can serve.
	gooberPinMissingCode = "gate_pin_missing"
	// gooberPinUnverifiableCode names a pin this worker could not even
	// evaluate, because no tree it holds could be compiled into goober
	// digests at all. Distinct from missing on purpose: "your kit is not
	// here" and "I cannot tell what is here" call for different operator
	// actions, and collapsing them would send an operator hunting a config
	// rollout when the real fault is a tree that does not compile.
	gooberPinUnverifiableCode = "run_pin_unverifiable"
)

// gooberPinRefusal is the worker's refusal to execute an attempt against a
// goober kit the run was not admitted against.
//
// It carries content digests and configuration NAMES only. That is a hard
// property, not an accident: this string is returned through the activity
// failure into Temporal history and the run journal, both of which are read by
// more people than the config tree is. Nothing here interpolates a config
// value, a credential ref, a binding, or an instruction body — a digest is a
// hash of them, and a hash is what makes the refusal actionable without
// republishing what it is a hash of.
type gooberPinRefusal struct {
	// Code is the named classification (gooberPinMissingCode /
	// gooberPinUnverifiableCode).
	Code string
	// Gaggle and Workflow identify what was being resolved.
	Gaggle   string
	Workflow string
	// Expected is the digest the run pinned.
	Expected string
	// Served are the digests this worker's current and retained trees do
	// resolve for the same identity, newest first — the "so what have you
	// got" an operator asks next.
	Served []string
	// Detail explains an unverifiable tree. Empty for a plain mismatch.
	Detail string
}

func (e *gooberPinRefusal) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "worker: %s: run pins goober digest %s for gaggle %q workflow %q, ",
		e.Code, e.Expected, e.Gaggle, e.Workflow)
	switch {
	case len(e.Served) == 0 && e.Detail != "":
		fmt.Fprintf(&b, "and this worker cannot resolve a goober digest from any config tree it holds (%s)", e.Detail)
	case len(e.Served) == 0:
		b.WriteString("and this worker holds no config tree that resolves one")
	default:
		fmt.Fprintf(&b, "but this worker's config trees resolve %s", strings.Join(e.Served, ", "))
	}
	b.WriteString("; refusing to substitute the currently-configured goober. " +
		"This worker's config tree is behind (or ahead of) the tree the run was admitted against: " +
		"the attempt retries and recovers once a config reload brings the pinned tree into force (#3884).")
	return b.String()
}

// refuseGooberPin marks a refusal as INFRASTRUCTURE class before it leaves the
// seam.
//
// The class is the load-bearing half. classifySeamError maps an
// invoke.InfrastructureFailure onto engine.FailureTypeInfrastructure, which
// dispatchWithRetry retries out of the bounded infrastructure budget instead
// of the stage's policy/repass budget. That is the correct accounting: the
// AGENT did nothing wrong, and charging a repass to the worker's config lag
// would exhaust a run's real retries on a condition a reload fixes by itself.
// It is also what makes "retry after reload recovers" true rather than
// aspirational — a policy-classed or non-retryable refusal would end the run
// before the reload it is waiting for could land.
func refuseGooberPin(refusal *gooberPinRefusal) error {
	return invoke.InfrastructureFailure(refusal)
}

// gooberDigestIndex is one config snapshot's (gaggle, workflow) →
// goober-digest table, computed at most once and only if something actually
// asks. Lazy because the compile it runs is not free and an unpinned worker
// must not pay for it; once because every attempt on a busy worker would
// otherwise recompute the same table.
type gooberDigestIndex struct {
	once       sync.Once
	compute    func() (map[localscheduler.WorkflowIdentity]string, error)
	byIdentity map[localscheduler.WorkflowIdentity]string
	err        error
}

func (i *gooberDigestIndex) resolve() (map[localscheduler.WorkflowIdentity]string, error) {
	i.once.Do(func() {
		if i.compute == nil {
			i.err = errors.New("no goober digest computation is wired for this config snapshot")
			return
		}
		i.byIdentity, i.err = i.compute()
	})
	return i.byIdentity, i.err
}

// gooberDigestFor returns the digest this snapshot's tree resolves for one
// workflow identity.
func (s *workerConfigSnapshot) gooberDigestFor(gaggle, workflowName string) (string, error) {
	if s.digests == nil {
		return "", errors.New("config snapshot carries no goober digest index")
	}
	byIdentity, err := s.digests.resolve()
	if err != nil {
		return "", err
	}
	digest, ok := byIdentity[localscheduler.WorkflowIdentity{Gaggle: gaggle, Workflow: workflowName}]
	if !ok {
		return "", fmt.Errorf("config tree %s declares no workflow %q in gaggle %q", s.digest, workflowName, gaggle)
	}
	return digest, nil
}

// instructionsFor narrows this snapshot's captured instruction bodies to one
// gaggle's goobers. It fails closed on a goober the snapshot has no
// instructions for rather than handing an executor a short map: an agentic
// stage built without its instructions is a stage running as nobody.
func (s *workerConfigSnapshot) instructionsFor(goobers map[string]apiv1.GooberSpec) (map[string]string, error) {
	out := make(map[string]string, len(goobers))
	for name := range goobers {
		content, ok := s.instructions[name]
		if !ok {
			return nil, &gooberInstructionsError{
				Goober: name,
				Err:    fmt.Errorf("config tree %s captured no instructions for this goober", s.digest),
			}
		}
		out[name] = content
	}
	return out, nil
}

// newGooberDigestIndex builds the lazy digest table for one just-read tree.
//
// It closes over the snapshot's OWN captured instructions and skill packages,
// never over the config directory, which is the property that makes a retained
// snapshot's answer a fact about the tree it captured rather than about
// whatever is mounted when the question is finally asked.
func newGooberDigestIndex(
	cfg *instance.Config,
	set *instance.ConfigSet,
	instructions map[string]string,
	skillPackages map[string]map[string][]workflow.SkillFile,
) *gooberDigestIndex {
	return &gooberDigestIndex{
		compute: func() (map[localscheduler.WorkflowIdentity]string, error) {
			_, digests, _, _, err := compiledMachinesWithGooberDigests(
				set,
				goobersByName(set),
				instructions,
				cfg.Runner.EnvPassthrough,
				cfg.Runner.HarnessCommand,
				// The daemon defers model discovery when it computes the
				// digests this pin is matched against. Deferring here too is
				// not an optimization: a worker that probed the launcher for a
				// model the daemon took from config would resolve a different
				// GooberSpec.Model, digest it, and refuse every attempt of
				// every run as a mismatch.
				true,
				func(gaggle string, _ map[string]apiv1.GooberSpec) (map[string][]workflow.SkillFile, error) {
					return skillPackages[gaggle], nil
				},
			)
			if err != nil {
				return nil, err
			}
			return digests, nil
		},
	}
}

// loadSnapshotGooberInputs reads the config-tree content a snapshot must
// CAPTURE to stay answerable after the tree moves: every configured goober's
// instruction body, and every gaggle's resolved skill packages.
//
// Read here, once per snapshot, rather than lazily at first use, because
// "lazily" would mean "from whatever is on disk by then" — the exact
// substitution the pin exists to prevent.
func loadSnapshotGooberInputs(configDir string, set *instance.ConfigSet) (map[string]string, map[string]map[string][]workflow.SkillFile, error) {
	goobers := goobersByName(set)
	instructions, err := loadGooberInstructions(configDir, goobers)
	if err != nil {
		return nil, nil, err
	}
	skillPackages := make(map[string]map[string][]workflow.SkillFile, len(set.Gaggles))
	for i := range set.Gaggles {
		gaggle := set.Gaggles[i].Name
		packages, err := loadGooberSkillPackages(configDir, gaggle, goobers)
		if err != nil {
			return nil, nil, err
		}
		skillPackages[gaggle] = packages
	}
	return instructions, skillPackages, nil
}

// retainSupersededLocked pushes the snapshot a reload just replaced onto the
// bounded history. Callers must hold w.mu.
//
// Eviction is by age, not by liveness, because the worker does not know which
// runs are in flight — the engine does, and asking it per reload would put a
// Temporal round trip on the config path. Age is the right proxy anyway: what
// history covers is "a reload landed mid-run", and a tree three reloads old is
// one no attempt should still be arriving for. The evicted tree's kits become
// unreachable and are collected; a pin for one refuses by name.
func (w *workerSeams) retainSupersededLocked(superseded *workerConfigSnapshot) {
	if superseded == nil || w.historyDepth <= 0 {
		w.history = nil
		return
	}
	retained := make([]*workerConfigSnapshot, 0, w.historyDepth)
	retained = append(retained, superseded)
	for _, entry := range w.history {
		if len(retained) == w.historyDepth {
			break
		}
		// A tree can be superseded and re-published (an edit reverted): keep
		// exactly one entry per digest so a flapping tree cannot evict the
		// whole history with copies of itself.
		if entry.digest == superseded.digest {
			continue
		}
		retained = append(retained, entry)
	}
	w.history = retained
}

// forPinnedGaggle resolves the seams an attempt pinned to gooberDigest is
// entitled to, or refuses.
//
// An empty pin resolves the current tree through forGaggle unchanged: runs
// started before the pin existed, and every seam that executes no goober kit,
// must keep working exactly as they did.
func (w *workerSeams) forPinnedGaggle(gaggle, workflowName, pin string) (*gaggleSeams, error) {
	if pin == "" {
		return w.forGaggle(gaggle)
	}
	// Fast path: the overwhelmingly common case is a run pinned to the tree
	// the worker is already serving, whose kit is already built. Taking it
	// without the lock keeps the steady state exactly as cheap as it was
	// before the pin existed. The published snapshot is immutable, so a hit
	// here is whole-tree consistent.
	if snapshot := w.snapshot.Load(); snapshot != nil {
		if built, ok := snapshot.gaggles[gaggle]; ok {
			if digest, err := snapshot.gooberDigestFor(gaggle, workflowName); err == nil && digest == pin {
				return built.seams, nil
			}
		}
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	current, err := w.currentSnapshotLocked()
	if err != nil {
		return nil, err
	}

	// Newest first: a pin that several retained trees resolve — a tree edited
	// and reverted — is served the most recent of them, which is the one a
	// freshly started run would also have been admitted against.
	candidates := make([]*workerConfigSnapshot, 0, 1+len(w.history))
	candidates = append(candidates, current)
	candidates = append(candidates, w.history...)

	var served []string
	var unverifiable error
	for _, snapshot := range candidates {
		digest, err := snapshot.gooberDigestFor(gaggle, workflowName)
		if err != nil {
			// A tree that cannot answer is not a tree that answers "no": keep
			// looking, and only report unverifiability if NOTHING answered.
			if unverifiable == nil {
				unverifiable = err
			}
			continue
		}
		if !containsString(served, digest) {
			served = append(served, digest)
		}
		if digest != pin {
			continue
		}
		return w.seamsFromLocked(snapshot, gaggle)
	}

	refusal := &gooberPinRefusal{
		Code:     gooberPinMissingCode,
		Gaggle:   gaggle,
		Workflow: workflowName,
		Expected: pin,
		Served:   served,
	}
	if len(served) == 0 && unverifiable != nil {
		refusal.Code = gooberPinUnverifiableCode
		refusal.Detail = unverifiable.Error()
	}
	return nil, refuseGooberPin(refusal)
}

// seamsFromLocked returns the gaggle's kit as built from one specific
// snapshot, building it on first use. Callers must hold w.mu.
//
// A newly built kit is published back into the snapshot it belongs to — by
// COPY, never by mutation, so a concurrent holder of the old pointer keeps its
// whole-tree-consistent view — whether that snapshot is the current one or a
// retained one. Publishing it back into a retained tree matters: an in-flight
// run whose first attempt lands only after a reload builds its kit from the
// retained tree, and every later attempt of that run must reuse it rather than
// re-running harness preflight and credential resolution per attempt.
func (w *workerSeams) seamsFromLocked(snapshot *workerConfigSnapshot, gaggle string) (*gaggleSeams, error) {
	if built, ok := snapshot.gaggles[gaggle]; ok {
		return built.seams, nil
	}
	built, err := w.buildGaggleSeams(snapshot, gaggle)
	if err != nil {
		return nil, err
	}
	updated := snapshot.withGaggle(gaggle, built)
	if current := w.snapshot.Load(); current == snapshot {
		w.snapshot.Store(updated)
		return built.seams, nil
	}
	for i, entry := range w.history {
		if entry == snapshot {
			w.history[i] = updated
			break
		}
	}
	return built.seams, nil
}

// snapshotForPin returns the whole config snapshot an attempt pinned to
// gooberDigest is entitled to read its configuration from — the same
// resolution forPinnedGaggle performs, for the callers that need the TREE
// rather than the built executors (the mode-3 agentic kit writer, which
// publishes one goober's instructions to a stage pod).
func (w *workerSeams) snapshotForPin(gaggle, workflowName, pin string) (*workerConfigSnapshot, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	current, err := w.currentSnapshotLocked()
	if err != nil {
		return nil, err
	}
	if pin == "" {
		return current, nil
	}
	candidates := make([]*workerConfigSnapshot, 0, 1+len(w.history))
	candidates = append(candidates, current)
	candidates = append(candidates, w.history...)

	var served []string
	var unverifiable error
	for _, snapshot := range candidates {
		digest, err := snapshot.gooberDigestFor(gaggle, workflowName)
		if err != nil {
			if unverifiable == nil {
				unverifiable = err
			}
			continue
		}
		if !containsString(served, digest) {
			served = append(served, digest)
		}
		if digest == pin {
			return snapshot, nil
		}
	}
	refusal := &gooberPinRefusal{
		Code:     gooberPinMissingCode,
		Gaggle:   gaggle,
		Workflow: workflowName,
		Expected: pin,
		Served:   served,
	}
	if len(served) == 0 && unverifiable != nil {
		refusal.Code = gooberPinUnverifiableCode
		refusal.Detail = unverifiable.Error()
	}
	return nil, refuseGooberPin(refusal)
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
