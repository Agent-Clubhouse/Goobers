package journalpg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"

	"github.com/goobers/goobers/internal/journal"
)

type capture struct {
	mu              sync.Mutex
	backend         *Backend
	handle          journal.RunHandle
	owner           journal.OwnerToken
	id, stage, name string
	branch          int
	streams         map[string]*captureStream
	count           int
	failed          error
	final           *journal.Ref
	finalErr        error
	paths           []string
}
type captureStream struct {
	scrubber *journal.CheckpointScrubber
	offset   int
	dropped  int64
	previous string
}

// BeginTranscript allocates a private capture bound to this writer incarnation.
func (b *Backend) BeginTranscript(ctx context.Context, h journal.RunHandle, o journal.OwnerToken, branch int, stage, name string) (journal.ExpectedTranscriptCheckpointSession, error) {
	r, err := b.writeRun(h, o)
	if err != nil {
		return nil, err
	}
	if stage == "" || name == "" || len(stage) > 256 || len(name) > 256 {
		return nil, errors.New("journalpg: invalid transcript identity")
	}
	if _, err := journal.NewCheckpointScrubber(r.options.Scrubber); err != nil {
		return nil, err
	}
	if err := b.RepairAppendBoundary(ctx, h, o); err != nil {
		return nil, err
	}
	// Match the shared file-journal capture identity accepted by livejournal's
	// durable transcript adoption protocol.
	var identity [16]byte
	if _, err := rand.Read(identity[:]); err != nil {
		return nil, err
	}
	return &capture{backend: b, handle: h, owner: o, id: hex.EncodeToString(identity[:]), stage: stage, name: name, branch: branch, streams: map[string]*captureStream{}}, nil
}
func (c *capture) Append(delta journal.TranscriptCheckpoint) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.final != nil {
		return errors.New("journalpg: transcript already completed")
	}
	if c.failed != nil {
		return c.failed
	}
	c.failed = c.append(delta)
	return c.failed
}
func (c *capture) append(delta journal.TranscriptCheckpoint) error {
	r, err := c.backend.writeRun(c.handle, c.owner)
	if err != nil {
		return err
	}
	if c.count >= journal.MaxTranscriptCheckpoints || len(delta.CommitKey) > 256 {
		return errors.New("journalpg: transcript checkpoint limit exceeded")
	}
	if !validDelta(delta) {
		return errors.New("journalpg: invalid transcript stream/reason")
	}
	stream := c.streams[delta.Stream]
	if stream == nil {
		scrubber, err := journal.NewCheckpointScrubber(r.options.Scrubber)
		if err != nil {
			return err
		}
		stream = &captureStream{scrubber: scrubber}
		c.streams[delta.Stream] = stream
	}
	if delta.Offset != stream.offset || delta.DroppedBytes < stream.dropped {
		return errors.New("journalpg: discontinuous transcript cursor")
	}
	data, err := stream.scrubber.ScrubDelta(delta.Data)
	if err != nil {
		return err
	}
	ref, err := journal.SpanRef(data)
	if err != nil {
		return err
	}
	ref.Path = "spans/checkpoints/" + c.id + "/" + ref.Digest[len("sha256:"):]
	meta := map[string]any{"partial": true, "transcriptCapture": c.id, "transcriptStream": delta.Stream, "checkpoint": c.count, "sourceOffset": delta.Offset, "sourceBytes": len(delta.Data), "droppedBytes": delta.DroppedBytes, "reason": delta.Reason, "previousDigest": stream.previous}
	if delta.CommitKey != "" {
		meta["emitKey"] = delta.CommitKey
	}
	_, err = c.backend.recordBlob(context.Background(), r, journal.Event{Type: journal.EventSpanRecorded, Branch: c.branch, Stage: c.stage, Name: c.name + ".partial", Runner: meta}, preparedBlob{ref: ref, data: data})
	if err != nil {
		return err
	}
	c.paths = append(c.paths, ref.Path)
	stream.offset += len(delta.Data)
	stream.dropped = delta.DroppedBytes
	stream.previous = ref.Digest
	c.count++
	return nil
}
func validDelta(delta journal.TranscriptCheckpoint) bool {
	stream := delta.Stream == "process-output/1" || delta.Stream == "process-output/2" || delta.Stream == "copilot-session/0"
	reason := delta.Reason == "checkpoint" || delta.Reason == "process-exit" || delta.Reason == "canceled" || delta.Reason == "timeout"
	return stream && reason
}
func (c *capture) RecordFinal(schema string, data []byte) (journal.Ref, error) {
	return c.RecordFinalWithExpectedDigest(schema, data, "", "")
}
func (c *capture) RecordFinalWithExpectedDigest(schema string, data []byte, key, expected string) (journal.Ref, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, err := c.backend.writeRun(c.handle, c.owner)
	if err != nil {
		return journal.Ref{}, err
	}
	if len(key) > 256 {
		return journal.Ref{}, errors.New("journalpg: transcript final key exceeds limit")
	}
	if c.finalErr != nil {
		return journal.Ref{}, c.finalErr
	}
	if c.final != nil {
		if journal.Digest(r.options.Scrubber.Scrub(data)) != c.final.Digest {
			return journal.Ref{}, errors.New("journalpg: final transcript content changed")
		}
		return *c.final, c.backend.removeTranscriptPartials(context.Background(), r, c.paths)
	}
	meta := map[string]any{"transcriptCaptureComplete": c.id}
	if key != "" {
		meta["emitKey"] = key
	}
	ref, err := c.backend.recordSpan(context.Background(), r, journal.Event{Type: journal.EventSpanRecorded, Branch: c.branch, Stage: c.stage, Name: c.name, DataSchema: schema, Runner: meta}, data, expected)
	if err != nil {
		c.finalErr = err
		return journal.Ref{}, err
	}
	c.final = &ref
	// The completion event commits before private partial cleanup. Recovery
	// repeats this cleanup if a crash interrupts the second transaction.
	return ref, c.backend.removeTranscriptPartials(context.Background(), r, c.paths)
}

var _ journal.Backend = (*Backend)(nil)
var _ journal.ReaderBackend = (*Backend)(nil)
