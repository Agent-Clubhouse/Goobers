package childpod

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/goobers/goobers/internal/dispatcher"
)

// Normalize dynamic inputs before retention and transport. Typed join records
// become JSON objects without rounding integer values through float64.
func normalizeAttempt(a dispatcher.Attempt) (dispatcher.Attempt, error) {
	data, err := json.Marshal(a)
	if err != nil || len(data) > 2<<20 {
		return dispatcher.Attempt{}, errors.New("child attempt exceeds JSON transport bounds")
	}
	if a.Envelope == nil {
		return a, nil
	}
	var wire struct {
		Envelope struct{ Inputs map[string]any }
	}
	if err := decodeAttemptNumbers(data, &wire); err != nil {
		return a, err
	}
	envelope := *a.Envelope
	envelope.Inputs = wire.Envelope.Inputs
	a.Envelope = &envelope
	return a, nil
}

// Decode dynamic values separately from InvocationEnvelope.UnmarshalJSON,
// whose typed decode still enforces the closed workspace-revision contract.
func restoreRetainedInputs(data []byte, r *RetainedAttempt) error {
	var wire struct {
		Input struct {
			Attempt struct {
				Envelope struct{ Inputs map[string]any }
			}
		}
	}
	if err := decodeAttemptNumbers(data, &wire); err != nil {
		return err
	}
	if r.Input.Attempt.Envelope != nil {
		r.Input.Attempt.Envelope.Inputs = wire.Input.Attempt.Envelope.Inputs
	}
	return nil
}

func decodeAttemptNumbers(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return decoder.Decode(target)
}
