package journal

import (
	"bytes"
	"errors"
)

// CheckpointScrubber incrementally applies a scrubber chain without exposing
// unfinished matches across delta boundaries. It is owned by one capture
// stream and is not safe for concurrent use. Callers must bound capture bytes.
// Pending input stays in memory; it must never be persisted as a raw tail.
type CheckpointScrubber struct {
	stages []checkpointScrubStage
	bytes  int
}

// MaxCheckpointScrubBytes bounds raw input retained by a capture stream, even
// when an unfinished credential prevents any prefix from being released.
const MaxCheckpointScrubBytes = 16 << 20

type checkpointPrefixScrubber interface {
	Scrubber
	SafePrefix([]byte) int
}

type checkpointScrubStage struct {
	scrubber checkpointPrefixScrubber
	pending  []byte
}

// NewCheckpointScrubber refuses opaque scrubbers: Scrub alone cannot prove
// that a partial match is safe to persist. Each chain member receives the
// previous member's scrubbed output, just as in the ordinary whole-input path.
func NewCheckpointScrubber(scrubber Scrubber) (*CheckpointScrubber, error) {
	stream := &CheckpointScrubber{}
	if err := stream.add(scrubber); err != nil {
		return nil, err
	}
	if len(stream.stages) == 0 {
		return nil, errors.New("journal: checkpoint scrubber chain is empty")
	}
	return stream, nil
}

func (s *CheckpointScrubber) add(scrubber Scrubber) error {
	if chain, ok := scrubber.(multiScrubber); ok {
		for _, member := range chain {
			if err := s.add(member); err != nil {
				return err
			}
		}
		return nil
	}
	prefix, ok := scrubber.(checkpointPrefixScrubber)
	if !ok {
		return errors.New("journal: scrubber cannot safely checkpoint partial input")
	}
	s.stages = append(s.stages, checkpointScrubStage{scrubber: prefix})
	return nil
}

// ScrubDelta returns only newly safe, scrubbed bytes. Empty output is normal
// while a token or private-key block is incomplete. The final transcript still
// uses the ordinary whole-input scrubber; do not flush the pending raw suffix.
func (s *CheckpointScrubber) ScrubDelta(delta []byte) ([]byte, error) {
	if s == nil || len(s.stages) == 0 {
		return nil, errors.New("journal: checkpoint scrubber is not initialized")
	}
	if len(delta) > MaxCheckpointScrubBytes-s.bytes {
		return nil, errors.New("journal: transcript checkpoint capture exceeds byte limit")
	}
	s.bytes += len(delta)
	for i := range s.stages {
		stage := &s.stages[i]
		stage.pending = append(stage.pending, delta...)
		boundary := stage.scrubber.SafePrefix(stage.pending)
		delta = bytes.Clone(stage.scrubber.Scrub(stage.pending[:boundary]))
		stage.pending = bytes.Clone(stage.pending[boundary:])
	}
	return delta, nil
}

func (nopScrubber) SafePrefix(input []byte) int { return len(input) }

// SafePrefix accounts for both raw and JSON-escaped registered secrets. KMP
// cursors retain every possible partial suffix, including overlapping matches.
// Only positions where no target can continue are eligible checkpoint cuts.
// Credentials must be registered before the producer is allowed to use them,
// as required by the ordinary registry-scrubbing path too.
func (s *RegistryScrubber) SafePrefix(input []byte) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var targets []checkpointExactTarget
	for _, secret := range s.secrets {
		targets = append(targets, newCheckpointExactTarget(secret))
		for _, escaped := range jsonEscapedForms(secret) {
			targets = append(targets, newCheckpointExactTarget(escaped))
		}
	}
	safe := 0
	for offset, b := range input {
		live := false
		for i := range targets {
			target := &targets[i]
			target.advance(b)
			live = live || target.cursor != 0
		}
		if !live {
			safe = offset + 1
		}
	}
	return safe
}

type checkpointExactTarget struct {
	value  []byte
	fail   []int
	cursor int
}

func newCheckpointExactTarget(value []byte) checkpointExactTarget {
	fail := make([]int, len(value))
	for i, prefix := 1, 0; i < len(value); i++ {
		for prefix > 0 && value[i] != value[prefix] {
			prefix = fail[prefix-1]
		}
		if value[i] == value[prefix] {
			prefix++
		}
		fail[i] = prefix
	}
	return checkpointExactTarget{value: value, fail: fail}
}

func (t *checkpointExactTarget) advance(b byte) {
	for t.cursor > 0 && b != t.value[t.cursor] {
		t.cursor = t.fail[t.cursor-1]
	}
	if b == t.value[t.cursor] {
		t.cursor++
	}
	if t.cursor == len(t.value) {
		t.cursor = t.fail[t.cursor-1]
	}
}
