package eventing

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// ManifestInputName identifies service-authored provenance, separate from event
// payloads. Payload snapshots remain unapproved input even for signed producers.
const ManifestInputName = "event-consumer-manifest"

// Producer identifies an authenticated source; it is never parsed from event
// data or client-selected CloudEvents extension fields.
type Producer struct {
	Gaggle        string `json:"gaggle"`
	Binding       string `json:"binding"`
	Actor         string `json:"actor"`
	RunID         string `json:"runId,omitempty"`
	Stage         string `json:"stage,omitempty"`
	RootID        string `json:"rootId,omitempty"`
	RootGroupID   string `json:"rootGroupId,omitempty"`
	RootSetDigest string `json:"rootSetDigest,omitempty"`
	CausationID   string `json:"causationId,omitempty"`
	Depth         int    `json:"depth"`
}

// InputMember preserves the producer of each original member, including inputs
// suppressed by latest selection. Envelopes are separate immutable snapshots.
type InputMember struct {
	ReceiptID string   `json:"receiptId"`
	Sequence  int64    `json:"sequence"`
	Digest    string   `json:"digest"`
	Producer  Producer `json:"producer"`
	Selected  bool     `json:"selected"`
}

// InputManifest commits the exact input set and accepted actor to the run.
type InputManifest struct {
	Start        StartEnvelope `json:"start"`
	AcceptanceID string        `json:"acceptanceId"`
	Actor        string        `json:"actor"`
	Members      []InputMember `json:"members"`
}

// ExecutionInputs is a host-prepared bundle; only selected members have payload
// bytes. At most 1,000 bounded 16-KiB payloads can enter one run.
type ExecutionInputs struct {
	Manifest  InputManifest
	Envelopes map[string][]byte
}

// EventInputName is derived from declaration order, never external path data.
func EventInputName(index int) string { return fmt.Sprintf("event-%04d.json", index+1) }

// ParseInputManifest refuses unknown fields and ambiguous persisted metadata.
func ParseInputManifest(raw []byte) (InputManifest, error) {
	var m InputManifest
	err := decodeClosed(raw, 4<<20, &m)
	return m, err
}

// Validate checks complete immutable membership and normalized payload digests.
// The returned manifest is bounded to 4 MiB, independently of selected payloads.
func (in ExecutionInputs) Validate(runID, gaggle string) ([]byte, error) {
	m := in.Manifest
	if _, err := m.Start.Marshal(); err != nil {
		return nil, err
	}
	if !apiv1.ValidRunID(runID) || m.AcceptanceID != "trigger-"+runID || m.Start.Gaggle != gaggle || !boundedText(m.Actor, 1024) || len(m.Members) != m.Start.EventCount {
		return nil, errors.New("eventing: invalid execution authority or membership")
	}
	if err := in.validateMembers(gaggle); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(m)
	if err != nil || len(raw) > 4<<20 {
		return nil, errors.New("eventing: event manifest exceeds bound")
	}
	return raw, nil
}

func (in ExecutionInputs) validateMembers(gaggle string) error {
	seen := map[string]bool{}
	selected := 0
	var sequence int64
	for _, member := range in.Manifest.Members {
		if !boundedText(member.ReceiptID, 128) || seen[member.ReceiptID] || member.Sequence <= sequence || member.Producer.Gaggle != gaggle || !member.Producer.Valid() {
			return errors.New("eventing: invalid ordered event member")
		}
		if !validDigest(member.Digest) {
			return errors.New("eventing: invalid event member digest")
		}
		seen[member.ReceiptID], sequence = true, member.Sequence
		want := in.Manifest.Start.InputMode == "all" || member.ReceiptID == in.Manifest.Start.SelectedReceipt
		if member.Selected != want {
			return errors.New("eventing: event input selection differs")
		}
		if !member.Selected {
			continue
		}
		selected++
		raw, ok := in.Envelopes[member.ReceiptID]
		parsed, err := Parse(raw)
		if !ok || err != nil || parsed.Digest != member.Digest || !bytes.Equal(parsed.JSON, raw) {
			return errors.New("eventing: event input custody differs")
		}
	}
	if selected == 0 || selected != len(in.Envelopes) {
		return errors.New("eventing: incomplete selected event inputs")
	}
	return nil
}

// Valid checks the bounded authenticated producer shape independently of data.
func (p Producer) Valid() bool {
	if !boundedText(p.Gaggle, 128) || !boundedText(p.Binding, 256) || !boundedText(p.Actor, 1024) || p.Depth < 0 || p.Depth > 8 {
		return false
	}
	for _, value := range []string{p.RunID, p.Stage, p.RootID, p.RootGroupID, p.CausationID} {
		if value != "" && !boundedText(value, 256) {
			return false
		}
	}
	if !p.validRootReference() {
		return false
	}
	return (p.RunID == "") == (p.Stage == "") && (p.CausationID == "") == (p.Depth == 0) && ((p.Depth == 0 && p.RunID == "") || p.RootID != "" || p.RootGroupID != "")
}

func validDigest(value string) bool {
	if len(value) != 71 || !strings.HasPrefix(value, "sha256:") || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value[7:])
	return err == nil
}

func (p Producer) validRootReference() bool {
	if p.RootGroupID == "" {
		return p.RootSetDigest == ""
	}
	return p.RootID == "" && p.RunID != "" && p.CausationID == p.RootGroupID && p.Depth > 0 && validDigest(p.RootSetDigest)
}
