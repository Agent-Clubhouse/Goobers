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
	if interval <= 0 {
		interval = DefaultTranscriptCheckpointInterval
	}
	worker := transcriptCheckpoints{stop: make(chan string), done: make(chan error, 1)}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		ticks := ticker.C
		state := transcriptCheckpointState{buffer: buffer, sink: sink}
		var err error
		for {
			select {
			case <-ticks:
				if err = state.capture("checkpoint"); err != nil {
					// Do not retry an uncertain durable write or repeatedly
					// copy the same growing transcript after storage fails.
					ticks = nil
				}
			case reason := <-worker.stop:
				if err == nil {
					err = state.capture(reason)
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
