package harness

import "time"

// DefaultTranscriptCheckpointInterval bounds the ordinary checkpoint cadence.
const DefaultTranscriptCheckpointInterval = time.Minute

// TranscriptDelta contains only newly retained raw process bytes. Offset counts
// source bytes, not redacted bytes. Reason is checkpoint, process-exit, canceled,
// or timeout; process-exit does not mean the higher-level stage succeeded.
type TranscriptDelta struct {
	Offset       int
	Data         []byte
	DroppedBytes int64
	Reason       string
}

// InvocationTranscriptDelta identifies an independent capture stream within an
// adapter run. Invocation is one-based (the recovery turn is 2). Source currently
// identifies combined process output; it must not be confused with the native
// session log. Offsets and dropped-byte counts are local to that stream.
type InvocationTranscriptDelta struct {
	Invocation int
	Source     string
	TranscriptDelta
}

func (r RunRequest) processTranscriptCheckpoint(invocation int) func(TranscriptDelta) error {
	if r.TranscriptCheckpoint == nil {
		return nil
	}
	return func(delta TranscriptDelta) error {
		return r.TranscriptCheckpoint(InvocationTranscriptDelta{
			Invocation:      invocation,
			Source:          "process-output",
			TranscriptDelta: delta,
		})
	}
}

type transcriptCheckpointState struct {
	buffer  *syncBuffer
	sink    func(TranscriptDelta) error
	offset  int
	dropped int64
}

func (s *transcriptCheckpointState) capture(reason string) error {
	data, next, dropped := s.buffer.delta(s.offset)
	if len(data) == 0 && dropped == s.dropped && reason == "checkpoint" {
		return nil
	}
	if err := s.sink(TranscriptDelta{Offset: s.offset, Data: data, DroppedBytes: dropped, Reason: reason}); err != nil {
		return err
	}
	s.offset, s.dropped = next, dropped
	return nil
}

type transcriptCheckpoints struct {
	stop chan string
	done chan error
}

func startTranscriptCheckpoints(buffer *syncBuffer, interval time.Duration, sink func(TranscriptDelta) error) transcriptCheckpoints {
	if sink == nil {
		return transcriptCheckpoints{}
	}
	state := transcriptCheckpointState{buffer: buffer, sink: sink}
	return startTranscriptCheckpointWorker(interval, state.capture)
}

func startTranscriptCheckpointWorker(interval time.Duration, capture func(string) error) transcriptCheckpoints {
	if interval <= 0 {
		interval = DefaultTranscriptCheckpointInterval
	}
	worker := transcriptCheckpoints{stop: make(chan string), done: make(chan error, 1)}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		ticks := ticker.C
		var err error
		for {
			select {
			case <-ticks:
				if err = capture("checkpoint"); err != nil {
					// Do not retry an uncertain durable write or repeatedly
					// copy the same growing transcript after storage fails.
					ticks = nil
				}
			case reason := <-worker.stop:
				if err == nil {
					err = capture(reason)
				}
				worker.done <- err
				return
			}
		}
	}()
	return worker
}

func (c transcriptCheckpoints) finish(reason string) error {
	if c.stop == nil {
		return nil
	}
	c.stop <- reason
	return <-c.done
}

func transcriptEndReason(timedOut, canceled bool) string {
	if timedOut {
		return "timeout"
	}
	if canceled {
		return "canceled"
	}
	return "process-exit"
}
