package workerhost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/journal"
)

// StagingArtifacts is an ArtifactRecorder for a worker that does NOT own the
// run's journal.
//
// Tier 1 satisfies the executor's recorder with the run's *journal.Run: the
// runner mints the run, so it holds the journal open for the run's lifetime and
// artifacts land in it directly. A Temporal worker is in a different position.
// It executes a stage for a run it did not mint, and the engine's projection is
// what authors that run's journal afterward, from Temporal history.
//
// Two things follow, and they are why this type exists rather than the worker
// calling journal.Create:
//
//  1. The worker cannot author the journal without inventing RunIdentity fields
//     it does not have — WorkflowVersion, WorkflowDigest, the Machine digest.
//     Those are conformance-normative. Fabricating them would be worse than not
//     writing a journal at all.
//  2. journal.Create fails closed if the run directory already exists, so a
//     worker that created one would break the projection that runs later.
//
// So the worker content-addresses stage artifacts into a staging area beside
// the runs tree and hands back real journal.Refs. The bytes are durable and
// digest-addressed with the same scheme the journal uses, which is what lets a
// projection adopt them later rather than re-derive them.
//
// THE REMAINING GAP, stated plainly: nothing adopts these yet. The engine's
// projection carries only context manifests and gate verdicts — it has no
// channel for executor-produced bytes, and no span kind at all for transcripts.
// Until that channel exists, these artifacts are durable and reachable on disk
// but do not appear in the projected journal. That is the artifact/transcript
// projection gap, not something this type can close on its own.
type StagingArtifacts struct {
	// root is the per-run staging root, e.g. <runs>/.staging-artifacts/<runID>.
	root string
	// Scrubber redacts secrets before bytes are written, matching the journal's
	// own guarantee that a recorded artifact is scrubbed at rest.
	Scrubber journal.Scrubber
	// Store, when set, receives every recorded blob as well — the write-through
	// half of the distributed data plane (see workerhost.MaterializeContext for
	// the fetch half). Nil keeps the recorder purely local, which is the tier-1
	// shape and the default.
	//
	// Write-through happens AFTER the local write and its failure is not fatal:
	// a stage that produced its artifact correctly has not failed because the
	// fleet store was briefly unreachable, and the blob is content-addressed, so
	// a later Put of the same digest is indistinguishable from this one. What
	// is lost is only the ability of a LATER stage on a DIFFERENT node to read
	// it, and that surfaces there, as an integrity fault, with the digest in
	// hand.
	Store blobstore.Store

	mu      sync.Mutex
	putErrs []error
}

// StoreErrors returns write-through failures observed so far, for a caller that
// wants to log them. They are not returned from Record* on purpose: see Store.
func (s *StagingArtifacts) StoreErrors() []error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]error(nil), s.putErrs...)
}

// NewStagingArtifacts returns a recorder rooted at dir, optionally writing
// through to a fleet-wide store.
func NewStagingArtifacts(dir string, scrubber journal.Scrubber, store blobstore.Store) *StagingArtifacts {
	return &StagingArtifacts{root: dir, Scrubber: scrubber, Store: store}
}

// StagingArtifactsDir is the staging root for runID beneath a runs directory.
// It is a dot-prefixed sibling of the run directories so it can never collide
// with a run id (run ids are validated hex) and is skipped by anything that
// enumerates runs.
func StagingArtifactsDir(runsDir, runID string) string {
	return filepath.Join(runsDir, ".staging-artifacts", runID)
}

// Dir is the per-run root a same-run ContextPointer resolves against.
//
// THIS IS THE DISTRIBUTION GAP, and it is worth being exact about, because it
// is not the one I expected to find. A stage consumes prior work through
// ContextPointers, and harness.Executor.materializeContext resolves each one
// with cp.Artifact.Resolve(contextResolver.Dir()) — a read of
// <journalRoot>/<ref.Path> on the LOCAL FILESYSTEM. So the engine's entire
// inter-stage data channel is a path relative to a per-run directory that
// whichever process runs the stage is assumed to be able to open.
//
// Locally that assumption holds trivially: one runner, one directory. Across
// nodes it does not hold at all. Stage 1 records a blob into node A's staging
// dir; stage 2 is polled by node B, whose staging dir for the same run is
// empty; Resolve returns a missing file and the executor fails closed with an
// integrity fault. Correct behaviour, useless outcome.
//
// It is the same defect class as claims.json+flock — coordination through a
// filesystem two processes are assumed to share — and it is the reason PLACEMENT
// spanning operating systems (engine.stageTaskQueue) is not yet the same thing
// as WORK spanning them. Placement is done. The data plane is not.
//
// The shape of the fix is already implied by the refs: they are sha256
// digests, so the staging dir wants to be a CACHE in front of a shared
// content-addressed store, with the worker fetching any pointer digest it does
// not hold before the executor resolves it. That leaves Resolve, the journal,
// and tier 1 completely untouched.
//
// Until then this returns the local staging root, which resolves correctly for
// any pointer THIS worker produced and fails closed for any it did not.
func (s *StagingArtifacts) Dir() string {
	if s == nil {
		return ""
	}
	return s.root
}

// RecordSpanWithSchema records a within-stage trace span — in practice the
// AGENT TRANSCRIPT (goobers.dev/telemetry/genai-event/v1 blobs). Stored under
// spans/ rather than artifacts/, matching journal.Run's layout so a projection
// can adopt these by digest.
//
// Required for the same reason the bounded recorder is: the harness executor
// type-asserts its recorder to harness.SpanRecorder and refuses to construct
// without it — "runner artifact recorder does not implement
// harness.SpanRecorder" — so an agentic stage fails at construction.
//
// Note what this makes concrete: transcripts DO have a durable home on the
// engine path now. What they still lack is a way into the projected journal —
// the projection whitelist has no span kind at all, so these blobs are on disk
// and digest-addressed but unreferenced. That is the remaining half of the
// artifact/transcript projection gap.
func (s *StagingArtifacts) RecordSpanWithSchema(stage, name, dataSchema string, data []byte) (journal.Ref, error) {
	return s.recordUnder("spans", name, data, apiv1.IntegrityDerived, true)
}

// RecordArtifactBounded is RecordArtifact with a byte limit applied AFTER
// scrubbing, at the same boundary that writes and digests the blob — matching
// journal.Run so a truncated artifact still has a digest that commits to the
// bytes actually stored.
//
// Required, not optional: internal/executor's external-telemetry path type-
// asserts its recorder to BoundedArtifactRecorder and refuses to construct
// without it ("external telemetry journal must support bounded artifacts"). A
// recorder that implements only RecordArtifact fails at executor construction,
// not at first use.
func (s *StagingArtifacts) RecordArtifactBounded(name string, data []byte, maxBytes int) (journal.Ref, error) {
	return s.RecordArtifactBoundedWithIntegrity(name, data, apiv1.IntegrityDerived, maxBytes)
}

// RecordArtifactBoundedWithIntegrity records a size-bounded artifact with
// explicit provenance.
func (s *StagingArtifacts) RecordArtifactBoundedWithIntegrity(name string, data []byte, integrity apiv1.Integrity, maxBytes int) (journal.Ref, error) {
	if maxBytes <= 0 {
		return journal.Ref{}, fmt.Errorf("workerhost: artifact %q byte limit must be positive", name)
	}
	if s != nil && s.Scrubber != nil {
		data = s.Scrubber.Scrub(data)
	}
	if len(data) > maxBytes {
		data = data[:maxBytes]
	}
	// Already scrubbed above; record with a nil-scrub path so the digest
	// commits to exactly these bytes.
	return s.record(name, data, integrity, false)
}

// RecordArtifactWithIntegrity records an artifact with explicit provenance.
func (s *StagingArtifacts) RecordArtifactWithIntegrity(name string, data []byte, integrity apiv1.Integrity) (journal.Ref, error) {
	return s.record(name, data, integrity, true)
}

// RecordArtifact scrubs data, stores it by content digest, and returns a Ref.
// The digest commits to the SCRUBBED bytes, matching journal.Run.RecordArtifact
// so a Ref produced here is indistinguishable from one produced by the journal.
func (s *StagingArtifacts) RecordArtifact(name string, data []byte) (journal.Ref, error) {
	return s.record(name, data, apiv1.IntegrityDerived, true)
}

func (s *StagingArtifacts) record(name string, data []byte, integrity apiv1.Integrity, scrub bool) (journal.Ref, error) {
	return s.recordUnder("artifacts", name, data, integrity, scrub)
}

func (s *StagingArtifacts) recordUnder(kind, name string, data []byte, integrity apiv1.Integrity, scrub bool) (journal.Ref, error) {
	if s == nil || s.root == "" {
		return journal.Ref{}, fmt.Errorf("workerhost: staging artifacts not configured")
	}
	if scrub && s.Scrubber != nil {
		data = s.Scrubber.Scrub(data)
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	rel := filepath.Join(kind, "sha256", digest[:2], digest[2:])

	s.mu.Lock()
	defer s.mu.Unlock()
	abs := filepath.Join(s.root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return journal.Ref{}, fmt.Errorf("workerhost: create artifact dir: %w", err)
	}
	// Identical content deduplicates to one blob, as the journal's store does —
	// so a retry does not rewrite bytes that are already correct.
	if _, err := os.Stat(abs); err != nil {
		if !os.IsNotExist(err) {
			return journal.Ref{}, fmt.Errorf("workerhost: stat artifact: %w", err)
		}
		if err := os.WriteFile(abs, data, 0o644); err != nil {
			return journal.Ref{}, fmt.Errorf("workerhost: write artifact %q: %w", name, err)
		}
	}
	ref := journal.Ref{
		Path:      filepath.ToSlash(rel),
		Digest:    "sha256:" + digest,
		Size:      int64(len(data)),
		Integrity: integrity,
	}
	if s.Store != nil {
		if err := s.Store.Put(context.Background(), ref.Digest, data); err != nil {
			s.putErrs = append(s.putErrs, err)
		}
	}
	return ref, nil
}
