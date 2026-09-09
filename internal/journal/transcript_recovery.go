package journal

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
)

// recoverCompletedTranscripts closes the final-event-before-cleanup crash
// window. The caller holds the run writer lock and supplies its authoritative
// event snapshot. No terminal marker means no deletion, even for a dead run.
func (r *Run) recoverCompletedTranscripts(events []Event) error {
	completed := make(map[string]*TranscriptCapture)
	for _, event := range events {
		id, ok := event.Runner["transcriptCaptureComplete"].(string)
		if !ok || event.Type != EventSpanRecorded {
			continue
		}
		if !validTranscriptCaptureID(id) || event.Ref == nil {
			return errors.New("journal: invalid completed transcript capture")
		}
		if previous := completed[id]; previous != nil {
			return errors.New("journal: duplicate completed transcript capture")
		}
		ref := *event.Ref
		completed[id] = &TranscriptCapture{run: r, id: id, stage: event.Stage, name: event.Name,
			final: &ref, blobs: make(map[string]struct{})}
	}
	for _, event := range events {
		id, _ := event.Runner["transcriptCapture"].(string)
		capture := completed[id]
		if capture == nil {
			continue
		}
		if err := capture.adoptPartialEvent(event); err != nil {
			return err
		}
	}
	for _, capture := range completed {
		if err := capture.recoverCleanup(); err != nil {
			return err
		}
	}
	return nil
}

func validTranscriptCaptureID(id string) bool {
	if len(id) != 32 {
		return false
	}
	decoded, err := hex.DecodeString(id)
	return err == nil && hex.EncodeToString(decoded) == id
}

func (c *TranscriptCapture) adoptPartialEvent(event Event) error {
	if event.Type != EventSpanRecorded || event.Ref == nil || event.Runner["partial"] != true ||
		event.Stage != c.stage || event.Name != c.name+".partial" || c.count >= MaxTranscriptCheckpoints {
		return errors.New("journal: invalid superseded transcript checkpoint")
	}
	hexDigest, err := digestHex(event.Ref.Digest)
	if err != nil {
		return err
	}
	expected := path.Join(dirSpans, "checkpoints", c.id, hexDigest)
	if event.Ref.Path != expected {
		return errors.New("journal: superseded checkpoint is outside its capture namespace")
	}
	c.blobs[expected] = struct{}{}
	c.count++
	return nil
}

func (c *TranscriptCapture) recoverCleanup() error {
	directory := filepath.Join(c.run.dir, dirSpans, "checkpoints", c.id)
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil // Normal completion already cleaned this capture.
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("journal: transcript capture directory changed type")
	}
	expected, err := spanPath(c.final.Digest)
	if err != nil || c.final.Path != expected {
		return errors.New("journal: completed transcript does not name a canonical span")
	}
	reader := &Reader{dir: c.run.dir}
	if _, err := reader.ArtifactBytesBounded(*c.final, MaxCheckpointScrubBytes); err != nil {
		return fmt.Errorf("journal: preserve partials because final transcript is unavailable: %w", err)
	}
	return c.removePartialBlobs()
}
