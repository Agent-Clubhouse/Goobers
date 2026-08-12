package workerhost

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
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
	// Dir is the per-run staging root, e.g. <runs>/.staging-artifacts/<runID>.
	Dir string
	// Scrubber redacts secrets before bytes are written, matching the journal's
	// own guarantee that a recorded artifact is scrubbed at rest.
	Scrubber journal.Scrubber

	mu sync.Mutex
}

// NewStagingArtifacts returns a recorder rooted at dir.
func NewStagingArtifacts(dir string, scrubber journal.Scrubber) *StagingArtifacts {
	return &StagingArtifacts{Dir: dir, Scrubber: scrubber}
}

// StagingArtifactsDir is the staging root for runID beneath a runs directory.
// It is a dot-prefixed sibling of the run directories so it can never collide
// with a run id (run ids are validated hex) and is skipped by anything that
// enumerates runs.
func StagingArtifactsDir(runsDir, runID string) string {
	return filepath.Join(runsDir, ".staging-artifacts", runID)
}

// RecordArtifact scrubs data, stores it by content digest, and returns a Ref.
// The digest commits to the SCRUBBED bytes, matching journal.Run.RecordArtifact
// so a Ref produced here is indistinguishable from one produced by the journal.
func (s *StagingArtifacts) RecordArtifact(name string, data []byte) (journal.Ref, error) {
	if s == nil || s.Dir == "" {
		return journal.Ref{}, fmt.Errorf("workerhost: staging artifacts not configured")
	}
	if s.Scrubber != nil {
		data = s.Scrubber.Scrub(data)
	}
	sum := sha256.Sum256(data)
	hex := hex.EncodeToString(sum[:])
	rel := filepath.Join("artifacts", "sha256", hex[:2], hex[2:])

	s.mu.Lock()
	defer s.mu.Unlock()
	abs := filepath.Join(s.Dir, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return journal.Ref{}, fmt.Errorf("workerhost: create artifact dir: %w", err)
	}
	// Identical content deduplicates to one blob, as the journal's store does —
	// so a re-run or a retry does not rewrite bytes that are already correct.
	if _, err := os.Stat(abs); err != nil {
		if !os.IsNotExist(err) {
			return journal.Ref{}, fmt.Errorf("workerhost: stat artifact: %w", err)
		}
		if err := os.WriteFile(abs, data, 0o644); err != nil {
			return journal.Ref{}, fmt.Errorf("workerhost: write artifact %q: %w", name, err)
		}
	}
	return journal.Ref{
		Path:      filepath.ToSlash(rel),
		Digest:    "sha256:" + hex,
		Size:      int64(len(data)),
		Integrity: apiv1.IntegrityDerived,
	}, nil
}
