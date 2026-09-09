package journal

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sync"
)

// MaxTranscriptCheckpoints bounds metadata even for a producer that emits
// empty deltas or changes its termination reason on every call.
const MaxTranscriptCheckpoints = 4096

// TranscriptCheckpointSession is the durable lifecycle shared by local and
// remote recorders. Append must acknowledge persisted bytes, not a queued or
// best-effort upload. RecordFinal must commit the final transcript before
// retiring any partials.
type TranscriptCheckpointSession interface {
	Append(TranscriptCheckpoint) error
	RecordFinal(schema string, data []byte) (Ref, error)
}

// OpenTranscriptCheckpoint exposes the local implementation through the same
// session contract used by recorder transports that do not own the run writer.
func (r *Run) OpenTranscriptCheckpoint(stage, name string, scrubber Scrubber) (TranscriptCheckpointSession, error) {
	return r.BeginTranscriptCaptureWithScrubber(stage, name, scrubber)
}

// TranscriptCheckpoint is raw incremental input from a single capture stream.
// Offset counts raw bytes, not redacted bytes; streams have independent cursors.
type TranscriptCheckpoint struct {
	Stream       string
	Offset       int
	Data         []byte
	DroppedBytes int64
	Reason       string
	// CommitKey is an optional transport idempotency key committed in the
	// same event as the bytes. It is not a second, separately written receipt.
	CommitKey string
}

// TranscriptCapture owns one stage invocation's partial transcript. Successful
// Append calls have durably recorded only newly safe, redacted bytes. A failed
// append poisons the capture: an uncertain commit must not be retried against a
// redaction cursor that already consumed input.
type TranscriptCapture struct {
	mu       sync.Mutex
	run      *Run
	id       string
	stage    string
	name     string
	streams  map[string]*transcriptCaptureStream
	count    int
	err      error
	blobs    map[string]struct{}
	final    *Ref
	finalErr error
	scrubber Scrubber
}

type transcriptCaptureStream struct {
	scrubber *CheckpointScrubber
	offset   int
	dropped  int64
	previous string
}

// BeginTranscriptCapture allocates a private identity, not a new run or journal.
// The private content-address namespace prevents eventual partial cleanup from
// deleting a blob shared with another capture or the final transcript.
func (r *Run) BeginTranscriptCapture(stage, name string) (*TranscriptCapture, error) {
	return r.BeginTranscriptCaptureWithScrubber(stage, name, nopScrubber{})
}

// BeginTranscriptCaptureWithScrubber preserves the executor-before-journal
// redaction order when their scrubbers differ.
func (r *Run) BeginTranscriptCaptureWithScrubber(stage, name string, scrubber Scrubber) (*TranscriptCapture, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrClosed
	}
	if stage == "" || name == "" || len(stage) > 256 || len(name) > 256 {
		return nil, errors.New("journal: invalid transcript capture identity")
	}
	combined := Chain(scrubber, r.scrubber)
	if _, err := NewCheckpointScrubber(combined); err != nil {
		return nil, err
	}
	var identity [16]byte
	if _, err := rand.Read(identity[:]); err != nil {
		return nil, err
	}
	return &TranscriptCapture{run: r, id: hex.EncodeToString(identity[:]), stage: stage, name: name,
		streams: make(map[string]*transcriptCaptureStream), blobs: make(map[string]struct{}), scrubber: combined}, nil
}

// Append commits a delta and its ordered span event before acknowledging it.
// At most three streams are supported: two process invocations and one native
// Copilot log spanning those invocations. No raw suffix is written to disk.
func (c *TranscriptCapture) Append(delta TranscriptCheckpoint) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.final != nil {
		return errors.New("journal: transcript capture already completed")
	}
	if c.err != nil {
		return c.err
	}
	c.err = c.append(delta)
	return c.err
}

func (c *TranscriptCapture) append(delta TranscriptCheckpoint) error {
	if c.count >= MaxTranscriptCheckpoints {
		return errors.New("journal: transcript checkpoint count exceeds limit")
	}
	if !validTranscriptStream(delta.Stream) || !validTranscriptReason(delta.Reason) {
		return errors.New("journal: invalid transcript checkpoint stream or reason")
	}
	if len(delta.CommitKey) > 256 {
		return errors.New("journal: transcript checkpoint commit key exceeds limit")
	}
	stream := c.streams[delta.Stream]
	if stream == nil {
		scrubber, err := NewCheckpointScrubber(c.scrubber)
		if err != nil {
			return err
		}
		stream = &transcriptCaptureStream{scrubber: scrubber}
		c.streams[delta.Stream] = stream
	}
	if delta.Offset != stream.offset || delta.DroppedBytes < stream.dropped {
		return errors.New("journal: transcript checkpoint cursor is discontinuous")
	}
	clean, err := stream.scrubber.ScrubDelta(delta.Data)
	if err != nil {
		return err
	}
	meta := map[string]any{
		"partial": true, "transcriptCapture": c.id, "transcriptStream": delta.Stream,
		"checkpoint": c.count, "sourceOffset": delta.Offset, "sourceBytes": len(delta.Data),
		"droppedBytes": delta.DroppedBytes, "reason": delta.Reason, "previousDigest": stream.previous,
	}
	if delta.CommitKey != "" {
		meta["emitKey"] = delta.CommitKey
	}
	ref, err := c.run.recordTranscriptCheckpoint(c, clean, meta)
	if err != nil {
		return err
	}
	stream.offset += len(delta.Data)
	stream.dropped = delta.DroppedBytes
	stream.previous = ref.Digest
	c.count++
	return nil
}

func validTranscriptStream(stream string) bool {
	return stream == "process-output/1" || stream == "process-output/2" || stream == "copilot-session/0"
}

func validTranscriptReason(reason string) bool {
	return reason == "checkpoint" || reason == "process-exit" || reason == "canceled" || reason == "timeout"
}

func (r *Run) recordTranscriptCheckpoint(c *TranscriptCapture, data []byte, metadata map[string]any) (Ref, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return Ref{}, ErrClosed
	}
	digest := Digest(data)
	hexDigest, err := digestHex(digest)
	if err != nil {
		return Ref{}, err
	}
	relative := path.Join(dirSpans, "checkpoints", c.id, hexDigest)
	c.blobs[relative] = struct{}{}
	ref, err := writeContentScrubbed(r.dir, relative, data, digest)
	if err != nil {
		return Ref{}, fmt.Errorf("journal: write transcript checkpoint: %w", err)
	}
	// The blob writer flushes the capture directory. Newly created ancestors
	// must also reach disk before the event can durably point through them.
	for _, parent := range []string{path.Join(dirSpans, "checkpoints"), dirSpans, "."} {
		if err := fsyncDir(filepath.Join(r.dir, parent)); err != nil {
			return Ref{}, err
		}
	}
	if err := r.append(Event{Type: EventSpanRecorded, Stage: c.stage, Name: c.name + ".partial",
		Ref: &ref, Runner: metadata}); err != nil {
		return Ref{}, err
	}
	if err := r.checkpoint(); err != nil {
		return Ref{}, err
	}
	return ref, nil
}

// RecordFinal durably records the canonical transcript and its supersession
// marker before removing any partial bytes. A cleanup failure leaves the final
// artifact available and can be retried with identical content. Partial blobs
// are private to this capture, never shared with final content addresses.
func (c *TranscriptCapture) RecordFinal(schema string, data []byte) (Ref, error) {
	return c.RecordFinalWithCommitKey(schema, data, "")
}

// RecordFinalWithCommitKey binds remote finalization's retry key to the final
// span event, before cleanup. A retry after daemon restart can therefore be
// acknowledged from durable history without reopening a redaction session.
func (c *TranscriptCapture) RecordFinalWithCommitKey(schema string, data []byte, key string) (Ref, error) {
	return c.RecordFinalWithExpectedDigest(schema, data, key, "")
}

// RecordFinalWithExpectedDigest rejects a remote content-address mismatch before
// writing a final event or removing partials, using the same redaction pass that
// produces the persisted bytes. An empty digest disables the remote precondition.
func (c *TranscriptCapture) RecordFinalWithExpectedDigest(schema string, data []byte, key, expectedDigest string) (Ref, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(key) > 256 {
		return Ref{}, errors.New("journal: transcript final commit key exceeds limit")
	}
	if c.finalErr != nil {
		return Ref{}, c.finalErr
	}
	if c.final == nil {
		metadata := map[string]any{"transcriptCaptureComplete": c.id}
		if key != "" {
			metadata["emitKey"] = key
		}
		ref, err := c.run.recordSpanEventExpectedDigest(Event{Type: EventSpanRecorded, Stage: c.stage, Name: c.name,
			DataSchema: schema, Runner: metadata}, data, expectedDigest)
		if err != nil {
			// The event may already be durable even if its state checkpoint
			// failed. Do not emit a second final event on an uncertain retry.
			c.finalErr = err
			return Ref{}, err
		}
		c.final = &ref
	} else if Digest(c.run.scrubber.Scrub(data)) != c.final.Digest {
		return Ref{}, errors.New("journal: completed transcript content changed")
	}
	return *c.final, c.removePartialBlobs()
}

func (c *TranscriptCapture) removePartialBlobs() error {
	relativeDirectory := path.Join(dirSpans, "checkpoints", c.id)
	resolved, err := containedExistingBlobPath(c.run.dir, relativeDirectory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	resolvedRun, err := filepath.EvalSymlinks(c.run.dir)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(resolvedRun, resolved)
	if err != nil || filepath.ToSlash(relative) != relativeDirectory {
		return errors.New("journal: refusing redirected transcript cleanup directory")
	}
	var result error
	for relative := range c.blobs {
		if err := os.Remove(filepath.Join(c.run.dir, relative)); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}
	if result != nil {
		return result
	}
	directory := filepath.Join(c.run.dir, dirSpans, "checkpoints", c.id)
	if err := os.Remove(directory); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err // Never recursively remove unexpected content.
	}
	if len(c.blobs) == 0 {
		return nil
	}
	return fsyncDir(filepath.Dir(directory))
}
