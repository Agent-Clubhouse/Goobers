package livejournal

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"

	"github.com/goobers/goobers/internal/journal"
)

// MaxActiveTranscriptCaptures bounds per-run remote redaction state. Completed
// sessions will release their slot; losing a writer handle loses its sessions
// and requires a fresh capture rather than silently losing redaction context.
const MaxActiveTranscriptCaptures = 32

var ErrTranscriptSessionLost = errors.New("livejournal: transcript checkpoint session is unavailable; start a fresh capture")

// TranscriptCheckpointOp travels on the existing authenticated run-scoped
// journal plane. Data is already scrubbed by the worker; the daemon additionally
// applies its own streaming scrubber before durable publication.
type TranscriptCheckpointOp struct {
	Capture      string       `json:"capture"`
	Action       string       `json:"action"`
	Stage        string       `json:"stage"`
	Name         string       `json:"name"`
	Stream       string       `json:"stream,omitempty"`
	Offset       int          `json:"offset,omitempty"`
	Data         []byte       `json:"data,omitempty"`
	DroppedBytes int64        `json:"droppedBytes,omitempty"`
	Reason       string       `json:"reason,omitempty"`
	DataSchema   string       `json:"dataSchema,omitempty"`
	FinalRef     *journal.Ref `json:"finalRef,omitempty"`
}

type remoteTranscriptCapture struct {
	stage   string
	name    string
	capture *journal.TranscriptCapture
	next    int
}

func (w *Writer) applyTranscriptCheckpoint(ctx context.Context, run *liveRun, op Op) (bool, error) {
	request := op.Checkpoint
	if request == nil || !validRemoteCaptureID(request.Capture) {
		return false, errors.New("livejournal: invalid transcript checkpoint identity")
	}
	if request.Action == "open" {
		return run.openTranscriptCheckpoint(op, request)
	}
	if request.Action != "append" && request.Action != "final" {
		return false, errors.New("livejournal: unsupported transcript checkpoint action")
	}
	session := run.transcriptCaptures[request.Capture]
	if session == nil {
		return false, ErrTranscriptSessionLost
	}
	if session.stage != request.Stage || session.name != request.Name {
		return false, errors.New("livejournal: transcript checkpoint session identity mismatch")
	}
	if request.Action == "final" {
		return w.finalizeTranscriptCheckpoint(ctx, run, session, op)
	}
	if op.Key != request.Capture+"/checkpoint/"+strconv.Itoa(session.next) {
		return false, errors.New("livejournal: transcript checkpoint session or sequence mismatch")
	}
	if err := session.capture.Append(journal.TranscriptCheckpoint{Stream: request.Stream, Offset: request.Offset,
		Data: request.Data, DroppedBytes: request.DroppedBytes, Reason: request.Reason, CommitKey: op.Key}); err != nil {
		return false, err
	}
	session.next++
	run.keys[op.Key] = run.jr.Seq()
	return true, nil
}

func (w *Writer) finalizeTranscriptCheckpoint(ctx context.Context, run *liveRun, session *remoteTranscriptCapture, op Op) (bool, error) {
	request := op.Checkpoint
	if op.Key != request.Capture+"/final" || request.FinalRef == nil || len(request.Data) != 0 {
		return false, errors.New("livejournal: invalid transcript finalization")
	}
	if request.FinalRef.Size < 0 || request.FinalRef.Size > journal.MaxCheckpointScrubBytes {
		return false, errors.New("livejournal: final transcript exceeds capture limit")
	}
	data, err := w.fetchSpan(ctx, request.FinalRef.Digest)
	if err != nil {
		return false, err // Never retire partials on unavailable final bytes.
	}
	if int64(len(data)) != request.FinalRef.Size {
		return false, errors.New("livejournal: final transcript size mismatch")
	}
	ref, err := session.capture.RecordFinalWithCommitKey(request.DataSchema, data, op.Key)
	if err != nil {
		return false, err
	}
	if ref.Digest != request.FinalRef.Digest {
		return false, errors.New("livejournal: final transcript changed at the daemon redaction boundary")
	}
	run.keys[op.Key] = run.jr.Seq()
	delete(run.transcriptCaptures, request.Capture)
	return true, nil
}

func (run *liveRun) openTranscriptCheckpoint(op Op, request *TranscriptCheckpointOp) (bool, error) {
	if op.Key != request.Capture+"/open" || len(request.Data) != 0 {
		return false, errors.New("livejournal: invalid transcript checkpoint open")
	}
	if len(run.transcriptCaptures) >= MaxActiveTranscriptCaptures {
		return false, errors.New("livejournal: active transcript checkpoint limit reached")
	}
	if run.transcriptCaptures[request.Capture] != nil {
		return false, errors.New("livejournal: transcript checkpoint session already exists")
	}
	capture, err := run.jr.BeginTranscriptCapture(request.Stage, request.Name)
	if err != nil {
		return false, err
	}
	if err := run.jr.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: request.Stage,
		Runner: map[string]any{EmitKeyRunnerField: op.Key, "transcriptCaptureOpened": request.Capture}}); err != nil {
		return false, fmt.Errorf("livejournal: persist transcript capture open: %w", err)
	}
	if run.transcriptCaptures == nil {
		run.transcriptCaptures = make(map[string]*remoteTranscriptCapture)
	}
	run.transcriptCaptures[request.Capture] = &remoteTranscriptCapture{stage: request.Stage, name: request.Name, capture: capture}
	run.keys[op.Key] = run.jr.Seq()
	return true, nil
}

func validRemoteCaptureID(id string) bool {
	if len(id) != 32 {
		return false
	}
	decoded, err := hex.DecodeString(id)
	return err == nil && hex.EncodeToString(decoded) == id
}
