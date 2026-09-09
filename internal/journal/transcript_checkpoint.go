package journal

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sync"
)

// MaxTranscriptCheckpoints bounds metadata even for a producer that emits
// empty deltas or changes its termination reason on every call.
const MaxTranscriptCheckpoints = 4096

// TranscriptCheckpoint is raw incremental input from a single capture stream.
// Offset counts raw bytes, not redacted bytes; streams have independent cursors.
type TranscriptCheckpoint struct {
	Stream       string
	Offset       int
	Data         []byte
	DroppedBytes int64
	Reason       string
}

// TranscriptCapture owns one stage invocation's partial transcript. Successful
// Append calls have durably recorded only newly safe, redacted bytes. A failed
// append poisons the capture: an uncertain commit must not be retried against a
// redaction cursor that already consumed input.
type TranscriptCapture struct {
	mu      sync.Mutex
	run     *Run
	id      string
	stage   string
	name    string
	streams map[string]*transcriptCaptureStream
	count   int
	err     error
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
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrClosed
	}
	if stage == "" || name == "" || len(stage) > 256 || len(name) > 256 {
		return nil, errors.New("journal: invalid transcript capture identity")
	}
	if _, err := NewCheckpointScrubber(r.scrubber); err != nil {
		return nil, err
	}
	var identity [16]byte
	if _, err := rand.Read(identity[:]); err != nil {
		return nil, err
	}
	return &TranscriptCapture{run: r, id: hex.EncodeToString(identity[:]), stage: stage, name: name,
		streams: make(map[string]*transcriptCaptureStream)}, nil
}

// Append commits a delta and its ordered span event before acknowledging it.
// At most three streams are supported: two process invocations and one native
// Copilot log spanning those invocations. No raw suffix is written to disk.
func (c *TranscriptCapture) Append(delta TranscriptCheckpoint) error {
	c.mu.Lock()
	defer c.mu.Unlock()
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
	stream := c.streams[delta.Stream]
	if stream == nil {
		scrubber, err := NewCheckpointScrubber(c.run.scrubber)
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
