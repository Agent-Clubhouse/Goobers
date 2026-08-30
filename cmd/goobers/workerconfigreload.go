package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

// workerConfigReloadInterval is the default cadence `goobers worker` re-reads
// its config tree at.
//
// WHY THE WORKER WATCHES ITS OWN TREE AT ALL (#3884).
//
// The worker used to build every agentic kit — goober instructions, gaggle
// config, credential grants — from the config tree as it stood when the
// process started, and never looked again. The daemon, by contrast, syncs its
// tree live. When the two diverged (Infra LEDGER I-51: a worker one Workflows
// revision behind the daemon for 32 minutes) every stage the worker served
// resolved credentials against the STALE gaggle, found no binding for the
// retargeted project, fell back to the first binding, and produced a
// wrong-credential clone failure — with nothing in run.yaml to show it.
//
// Polling, not fs notification, deliberately: the tree the worker reads is a
// mounted/seeded directory whose updates arrive as whole-directory swaps and
// atomic renames rather than as a predictable event stream, and inotify does
// not survive the symlink swap a projected ConfigMap volume performs. A cheap
// content digest (configDirectoryDigest — the daemon's own) makes the steady
// state a directory walk that changes nothing, and makes the change signal
// content-based rather than timestamp-based.
const workerConfigReloadInterval = 10 * time.Second

// workerReloadOutcome reports what a single reload check did. It is the
// worker's equivalent of configReloader.pollOnce's structured result: callers
// (the watcher's log line, tests) need to tell "nothing changed" from
// "applied" from "the tree was moving" without re-deriving any of it.
type workerReloadOutcome struct {
	// Digest is the config-tree digest this check observed.
	Digest string
	// Applied is true when a new snapshot was published.
	Applied bool
	// Unstable is true when the tree changed underneath the read, so nothing
	// was published and the next check converges instead. This is what keeps a
	// half-written tree from ever becoming a snapshot.
	Unstable bool
	// Invalidated names the gaggles whose built seams the new tree superseded.
	// Their kits are dropped, not mutated: an attempt already holding one
	// keeps it, and the NEXT attempt builds against the new tree.
	Invalidated []string
	// Retained names the gaggles the new tree did not touch. Their seams are
	// carried over by pointer — no rebuild, no repeated harness preflight.
	Retained []string
	// RetainedDigests names the superseded config trees this worker still
	// holds after the publish, newest first — the bounded set a run's pinned
	// goober digest can still be resolved against (#3884). Empty when
	// retention is disabled or nothing has been superseded yet.
	RetainedDigests []string
}

// currentSnapshotLocked returns the published snapshot, loading one on first
// use. Callers must hold w.mu.
//
// The first load deliberately does NOT require a stable tree: before this
// reloader existed, forGaggle read the tree directly with no stability check
// at all, and failing a stage because the tree happened to be mid-write would
// be a new failure mode rather than a fix. Reload, which has a good snapshot
// to fall back on, does require stability.
func (w *workerSeams) currentSnapshotLocked() (*workerConfigSnapshot, error) {
	if snapshot := w.snapshot.Load(); snapshot != nil {
		return snapshot, nil
	}
	snapshot, stable, err := w.loadConfigSnapshot()
	if err != nil {
		return nil, err
	}
	if !stable {
		w.log("worker config reload: initial config tree read at %s was not stable; the next reload converges", snapshot.digest)
	}
	w.snapshot.Store(snapshot)
	return snapshot, nil
}

// loadConfigSnapshot reads one whole view of the config tree and reports
// whether the tree held still for the entire read. It never publishes.
func (w *workerSeams) loadConfigSnapshot() (*workerConfigSnapshot, bool, error) {
	l := instance.NewLayout(w.root)
	digest, err := configDirectoryDigest(l.ConfigDir())
	if err != nil {
		return nil, false, fmt.Errorf("worker: digest config directory: %w", err)
	}
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		return nil, false, fmt.Errorf("worker: load instance config: %w", err)
	}
	// The validation report travels with the error rather than being dropped.
	// A worker that executed a stage against config carrying known problems,
	// silently, is worse than one that refuses and says which problems — and
	// this seam has no terminal to print to, so the issues are folded into the
	// returned error where the activity failure will carry them.
	// TestCallersDoNotDiscardConfigReports enforces this, and caught it here.
	set, report, err := loadConfigDirectory(l.ConfigDir())
	if err != nil {
		if issues := validationIssueSummary(report); issues != "" {
			return nil, false, fmt.Errorf("worker: load config directory: %w (%s)", err, issues)
		}
		return nil, false, fmt.Errorf("worker: load config directory: %w", err)
	}
	instance.ApplyGaggleCICommand(set)
	instance.ApplyGaggleOutboxMirror(set)

	// Capture — not reference — the config-tree content this snapshot's kits
	// and goober digests are derived from, so it stays answerable after the
	// tree on disk moves past it (#3884, snapshot retention).
	instructions, skillPackages, err := loadSnapshotGooberInputs(l.ConfigDir(), set)
	if err != nil {
		return nil, false, fmt.Errorf("worker: load goober kit content: %w", err)
	}

	// Read-validate-reread, the same stability check the daemon's reloader
	// uses: if the digest moved while the directory was being parsed, the
	// parse may have seen half of one tree and half of another.
	settled, err := configDirectoryDigest(l.ConfigDir())
	if err != nil {
		return nil, false, fmt.Errorf("worker: digest config directory: %w", err)
	}
	snapshot := &workerConfigSnapshot{
		digest:        digest,
		cfg:           cfg,
		set:           set,
		instructions:  instructions,
		skillPackages: skillPackages,
		digests:       newGooberDigestIndex(cfg, set, instructions, skillPackages),
		gaggles:       map[string]*builtGaggleSeams{},
	}
	return snapshot, settled == digest, nil
}

// reloadOnce runs exactly one reload check and publishes a new snapshot when
// the config tree has settled on new content.
//
// It is the whole reload mechanism; the watcher below only supplies a clock
// and a log destination. Errors are returned, never swallowed: an unparsable
// tree leaves the last-known-good snapshot published and in force, so the
// worker keeps serving the tree it last agreed with instead of failing every
// stage or — worse — silently accepting a broken one.
func (w *workerSeams) reloadOnce() (workerReloadOutcome, error) {
	dir := instance.NewLayout(w.root).ConfigDir()
	digest, err := configDirectoryDigest(dir)
	if err != nil {
		return workerReloadOutcome{}, fmt.Errorf("worker: digest config directory: %w", err)
	}
	// The steady state: same content, so nothing is parsed, nothing is
	// rebuilt, and no snapshot is published.
	if current := w.snapshot.Load(); current != nil && current.digest == digest {
		return workerReloadOutcome{Digest: digest}, nil
	}

	next, stable, err := w.loadConfigSnapshot()
	if err != nil {
		return workerReloadOutcome{Digest: digest}, err
	}
	if !stable || next.digest != digest {
		return workerReloadOutcome{Digest: next.digest, Unstable: true}, nil
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	current := w.snapshot.Load()
	if current == nil {
		w.snapshot.Store(next)
		return workerReloadOutcome{Digest: digest, Applied: true}, nil
	}
	if current.digest == digest {
		// forGaggle published this same tree while the parse was in flight.
		return workerReloadOutcome{Digest: digest}, nil
	}
	// One last digest, now that no other writer can publish: without it a
	// reload that parsed tree T1 could overwrite a snapshot forGaggle had
	// already published from a NEWER tree T2, walking the worker backwards.
	// A tree that has moved again converges on the next check instead.
	settled, err := configDirectoryDigest(dir)
	if err != nil {
		return workerReloadOutcome{Digest: digest}, fmt.Errorf("worker: digest config directory: %w", err)
	}
	if settled != digest {
		return workerReloadOutcome{Digest: settled, Unstable: true}, nil
	}

	outcome := workerReloadOutcome{Digest: digest, Applied: true}
	for gaggle, built := range current.gaggles {
		fingerprint, err := w.gaggleFingerprint(next, gaggle)
		// A fingerprint that cannot be computed against the new tree (the
		// gaggle was removed, its instructions file vanished) invalidates the
		// entry rather than failing the reload: the next attempt then rebuilds
		// and reports the real, specific error from forGaggle instead of this
		// goroutine guessing at one.
		if err != nil || fingerprint != built.fingerprint {
			outcome.Invalidated = append(outcome.Invalidated, gaggle)
			continue
		}
		next.gaggles[gaggle] = built
		outcome.Retained = append(outcome.Retained, gaggle)
	}
	slices.Sort(outcome.Invalidated)
	slices.Sort(outcome.Retained)
	// The tree being replaced is retained, bounded, so an in-flight run pinned
	// to it is still served ITS kit rather than these new instructions
	// (#3884). Done before the publish, under the same lock, so there is no
	// instant at which the superseded tree is neither current nor retained.
	w.retainSupersededLocked(current)
	outcome.RetainedDigests = w.historyDigestsLocked()
	// One pointer store publishes the new tree and every carried-over kit
	// together, so a concurrent reader sees whole-old or whole-new.
	w.snapshot.Store(next)
	return outcome, nil
}

// historyDigestsLocked lists the retained superseded trees, newest first.
// Callers must hold w.mu.
func (w *workerSeams) historyDigestsLocked() []string {
	out := make([]string, 0, len(w.history))
	for _, entry := range w.history {
		out = append(out, entry.digest)
	}
	return out
}

// gaggleFingerprint fingerprints everything one gaggle's seams are built from,
// as that gaggle would read it out of the given snapshot.
//
// Snapshot-sourced, not disk-sourced: comparing a candidate tree against a
// built kit has to compare the CANDIDATE's instruction bytes, and re-reading
// the directory here would compare whatever landed since instead — which
// could retain a kit across a change it does not reflect.
func (w *workerSeams) gaggleFingerprint(snapshot *workerConfigSnapshot, gaggle string) (string, error) {
	goobers, err := resolveGoobersForGaggle(snapshot.set, gaggle)
	if err != nil {
		return "", err
	}
	instructions, err := snapshot.instructionsFor(goobers)
	if err != nil {
		return "", err
	}
	return gaggleConfigFingerprint(snapshot.cfg, snapshot.set, gaggle, goobers, instructions)
}

// gaggleFingerprintInput is the exact input set buildGaggleSeams reads for one
// gaggle, in a shape that hashes deterministically.
//
// Scoping is deliberately asymmetric, and the asymmetry is the point: the
// gaggle-scoped inputs (this gaggle's own Gaggle document, the goobers visible
// to it, their instruction bodies) are filtered, so another gaggle's edit does
// not rebuild this one; the shared inputs (instance.yaml, the manifest, every
// workflow, every gaggle's branch namespace) are NOT filtered, because
// preflightHarnesses, validateStoredCopilotAuthBoundaries, and
// branchNamespacesByGaggle each read across gaggle boundaries. Over-including
// a shared input costs a needless rebuild; under-including one reintroduces
// exactly the silent staleness this reloader exists to remove.
type gaggleFingerprintInput struct {
	Config           *instance.Config            `json:"config"`
	Manifest         *apiv1.Manifest             `json:"manifest"`
	Gaggle           *apiv1.Gaggle               `json:"gaggle"`
	Goobers          map[string]apiv1.GooberSpec `json:"goobers"`
	Instructions     map[string]string           `json:"instructions"`
	Workflows        []apiv1.Workflow            `json:"workflows"`
	BranchNamespaces map[string]string           `json:"branchNamespaces"`
}

func gaggleConfigFingerprint(
	cfg *instance.Config,
	set *instance.ConfigSet,
	gaggle string,
	goobers map[string]apiv1.GooberSpec,
	instructions map[string]string,
) (string, error) {
	input := gaggleFingerprintInput{
		Config:           cfg,
		Manifest:         set.Manifest,
		Goobers:          goobers,
		Instructions:     instructions,
		BranchNamespaces: branchNamespacesByGaggle(set),
	}
	for i := range set.Gaggles {
		if set.Gaggles[i].Name == gaggle {
			input.Gaggle = &set.Gaggles[i]
			break
		}
	}
	// Config-directory load order is not a contract; sort so the same tree
	// always fingerprints the same way.
	input.Workflows = append(input.Workflows, set.Workflows...)
	slices.SortStableFunc(input.Workflows, func(a, b apiv1.Workflow) int {
		if c := strings.Compare(a.Spec.Gaggle, b.Spec.Gaggle); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})

	encoded, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("encode gaggle %q config fingerprint: %w", gaggle, err)
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// log writes a reload diagnostic. Never fatal: a worker that stopped serving
// because its config tree briefly went unreadable would turn a recoverable
// deploy race into an outage.
func (w *workerSeams) log(format string, args ...any) {
	if w.logf == nil {
		return
	}
	w.logf(format, args...)
}

// workerConfigWatcher re-reads the worker's config tree on an interval for as
// long as it runs, and stops for good when Stop returns.
type workerConfigWatcher struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// startWorkerConfigWatcher runs reloadOnce every interval until Stop. It
// follows the openPRLoop lifecycle shape — own context, own done channel, Stop
// waits — so shutdown is an observable fact rather than a hope: after Stop
// returns, the goroutine has exited and cannot publish another snapshot.
func startWorkerConfigWatcher(ctx context.Context, seams *workerSeams, interval time.Duration) *workerConfigWatcher {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	watcher := &workerConfigWatcher{cancel: cancel, done: done}
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		// lastFailure dedupes an unchanging complaint: a config tree that stays
		// broken must say so once, not once per tick, and must say so AGAIN if
		// the failure changes.
		var lastFailure string
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				outcome, err := seams.reloadOnce()
				if err != nil {
					if message := err.Error(); message != lastFailure {
						lastFailure = message
						seams.log("worker config reload: rejected: %s; keeping the last-known-good config tree", message)
					}
					continue
				}
				if lastFailure != "" {
					lastFailure = ""
					seams.log("worker config reload: config tree readable again at %s", outcome.Digest)
				}
				if !outcome.Applied {
					continue
				}
				seams.log(
					"worker config reload: applied config tree %s; rebuilding gaggle seams [%s] on next attempt, retained [%s]",
					outcome.Digest,
					strings.Join(outcome.Invalidated, " "),
					strings.Join(outcome.Retained, " "),
				)
			}
		}
	}()
	return watcher
}

// Stop cancels the watcher and waits for its goroutine to exit.
func (w *workerConfigWatcher) Stop() {
	if w == nil {
		return
	}
	w.cancel()
	<-w.done
}
