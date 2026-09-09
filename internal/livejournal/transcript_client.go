package livejournal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

type TranscriptEmitter interface {
	Emit(context.Context, EmitRequest) (EmitResponse, error)
}

type TranscriptBlobWriter interface {
	Put(context.Context, string, []byte) error
}

// TranscriptTransport provides durable checkpoints without a local run writer.
// One transport is scoped to a run; every opened session has a fresh identity.
type TranscriptTransport struct {
	RunID   string
	Gaggle  string
	Emitter TranscriptEmitter
	Blobs   TranscriptBlobWriter
}

type remoteTranscriptSession struct {
	mu              sync.Mutex
	transport       TranscriptTransport
	id, stage, name string
	scrubber        journal.Scrubber
	streams         map[string]*remoteTranscriptStream
	next            int
	err             error
	final           *journal.Ref
}

type remoteTranscriptStream struct {
	scrubber     *journal.CheckpointScrubber
	sourceOffset int
	remoteOffset int
}

func (t TranscriptTransport) OpenTranscriptCheckpoint(stage, name string, scrubber journal.Scrubber) (journal.TranscriptCheckpointSession, error) {
	if t.Emitter == nil || t.Blobs == nil || t.RunID == "" || t.Gaggle == "" {
		return nil, errors.New("livejournal: checkpoint transport is incomplete")
	}
	if _, err := journal.NewCheckpointScrubber(scrubber); err != nil {
		return nil, err
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	s := &remoteTranscriptSession{transport: t, id: hex.EncodeToString(id[:]), stage: stage, name: name,
		scrubber: scrubber, streams: make(map[string]*remoteTranscriptStream)}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.emit(ctx, "open", "open", TranscriptCheckpointOp{}); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *remoteTranscriptSession) emit(ctx context.Context, action, key string, payload TranscriptCheckpointOp) error {
	payload.Capture, payload.Action, payload.Stage, payload.Name = s.id, action, s.stage, s.name
	_, err := s.transport.Emitter.Emit(ctx, EmitRequest{RunID: s.transport.RunID, Gaggle: s.transport.Gaggle,
		Ops: []Op{{Kind: OpTranscriptCheckpoint, Key: s.id + "/" + key, Time: time.Now().UTC(), Checkpoint: &payload}}})
	return err
}

func (s *remoteTranscriptSession) Append(delta journal.TranscriptCheckpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if s.final != nil {
		return errors.New("livejournal: checkpoint session already finalized")
	}
	s.err = s.append(delta)
	return s.err
}

func (s *remoteTranscriptSession) append(delta journal.TranscriptCheckpoint) error {
	if delta.Stream != "process-output/1" && delta.Stream != "process-output/2" && delta.Stream != "copilot-session/0" {
		return errors.New("livejournal: invalid checkpoint capture stream")
	}
	stream := s.streams[delta.Stream]
	if stream == nil {
		scrubber, err := journal.NewCheckpointScrubber(s.scrubber)
		if err != nil {
			return err
		}
		stream = &remoteTranscriptStream{scrubber: scrubber}
		s.streams[delta.Stream] = stream
	}
	if delta.Offset != stream.sourceOffset {
		return errors.New("livejournal: checkpoint source cursor is discontinuous")
	}
	clean, err := stream.scrubber.ScrubDelta(delta.Data)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		if s.next >= journal.MaxTranscriptCheckpoints {
			return errors.New("livejournal: checkpoint count exceeds limit")
		}
		// Base64-encoded JSON remains below the journal plane's 4 MiB cap.
		length := min(len(clean), 1<<20)
		if err := s.emit(ctx, "append", "checkpoint/"+strconv.Itoa(s.next), TranscriptCheckpointOp{
			Stream: delta.Stream, Offset: stream.remoteOffset, Data: clean[:length],
			DroppedBytes: delta.DroppedBytes, Reason: delta.Reason,
		}); err != nil {
			return err
		}
		s.next++
		stream.remoteOffset += length
		clean = clean[length:]
		if len(clean) == 0 {
			break
		}
	}
	stream.sourceOffset += len(delta.Data)
	return nil
}

func (s *remoteTranscriptSession) RecordFinal(schema string, data []byte) (journal.Ref, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	clean := s.scrubber.Scrub(data)
	if len(clean) > journal.MaxCheckpointScrubBytes {
		return journal.Ref{}, errors.New("livejournal: final transcript exceeds limit")
	}
	ref, err := journal.SpanRef(clean)
	if err != nil {
		return journal.Ref{}, err
	}
	if s.final != nil {
		if s.final.Digest != ref.Digest {
			return journal.Ref{}, errors.New("livejournal: final transcript changed")
		}
		return *s.final, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.transport.Blobs.Put(ctx, ref.Digest, clean); err != nil {
		return journal.Ref{}, err
	}
	if err := s.emit(ctx, "final", "final", TranscriptCheckpointOp{DataSchema: schema, FinalRef: &ref}); err != nil {
		return journal.Ref{}, err
	}
	s.final = &ref
	return ref, nil
}
