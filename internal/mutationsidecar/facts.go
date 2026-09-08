package mutationsidecar

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/goobers/goobers/providers"
)

// Fact is a provider receipt as written by a stage's mutation recorder.
// Recovery must preserve the typed receipts, not reduce them to a URL.
type Fact struct {
	ReceiptID         string                       `json:"receiptId,omitempty"`
	LandingIntent     *providers.LandingIntent     `json:"landingIntent,omitempty"`
	QueueAdmission    *providers.QueueAdmission    `json:"queueAdmission,omitempty"`
	MergeConfirmation *providers.MergeConfirmation `json:"mergeConfirmation,omitempty"`
	Provider          string                       `json:"provider"`
	Kind              string                       `json:"kind"`
	ID                string                       `json:"id"`
	URL               string                       `json:"url,omitempty"`
	Operation         string                       `json:"operation,omitempty"`
	RunID             string                       `json:"runId,omitempty"`
	Outcome           string                       `json:"outcome,omitempty"`
	ErrorCode         string                       `json:"errorCode,omitempty"`
	ProviderRunID     string                       `json:"providerRunId,omitempty"`
}

// ParseRecoveryFacts is all-or-nothing: a cleanup handoff cannot discard a
// corrupt or newer receipt while treating its remaining prefix as complete.
// Unknown fields also fail closed so an older binary preserves newer proof.
func ParseRecoveryFacts(data []byte) ([]Fact, error) {
	if len(data) > MaxBytes {
		return nil, fmt.Errorf("mutation sidecar exceeds %d bytes", MaxBytes)
	}
	lines := bytes.Split(data, []byte{'\n'})
	if len(lines) > MaxLines+1 || (len(lines) == MaxLines+1 && len(lines[len(lines)-1]) != 0) {
		return nil, fmt.Errorf("mutation sidecar exceeds %d lines", MaxLines)
	}
	var facts []Fact
	for i, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		var fact Fact
		if err := decoder.Decode(&fact); err != nil {
			return nil, fmt.Errorf("mutation sidecar line %d: %w", i+1, err)
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return nil, fmt.Errorf("mutation sidecar line %d has trailing data", i+1)
		}
		if fact.Provider == "" || fact.Kind == "" || fact.ID == "" {
			return nil, fmt.Errorf("mutation sidecar line %d lacks provider, kind, or id", i+1)
		}
		facts = append(facts, fact)
	}
	return facts, nil
}
