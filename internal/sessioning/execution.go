package sessioning

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// ContextInputName is the immutable unapproved conversational input to a turn.
const ContextInputName = "interactive-session-context"

// ExecutionInputs reconstructs bounded conversation evidence. It makes no
// claim that a model-native session or its hidden state was resumed.
type ExecutionInputs struct {
	Version      int           `json:"version"`
	AcceptanceID string        `json:"acceptanceId"`
	Start        StartEnvelope `json:"start"`
	Messages     []Message     `json:"messages"`
	Truncated    bool          `json:"truncated"`
}

// Digest hashes retained source and exact queue/input payloads.
func Digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Validate binds a bounded context manifest to its actual accepted turn.
func (in ExecutionInputs) Validate(runID, gaggle string) ([]byte, error) {
	e := in.Start
	if in.Version != 1 || e.Kind != StartKind || e.Gaggle != gaggle || in.AcceptanceID != "trigger-"+runID || len(runID) != 32 || strings.Trim(runID, "0123456789abcdef") != "" || len(in.Messages) == 0 || len(in.Messages) > MaxContextMessages {
		return nil, errors.New("session execution identity is invalid")
	}
	if err := validStartDigests(e); err != nil {
		return nil, err
	}
	last := in.Messages[len(in.Messages)-1]
	if last.ID != e.MessageID || last.TurnID != e.TurnID || last.SessionID != e.SessionID || last.ActorKind != "human" || last.Actor == nil || last.RunID != "" || last.Outcome != "" || MessageDigest(last.Text, last.RepairTarget) != e.MessageDigest {
		return nil, errors.New("session current message does not match accepted input")
	}
	for _, m := range in.Messages {
		if !validContextMessage(m, e.SessionID) {
			return nil, errors.New("session context message is invalid")
		}
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxContextBytes {
		return nil, errors.New("session context exceeds limit")
	}
	return raw, nil
}

// ParseExecutionInputs rejects unknown fields and noncanonical durable bytes.
func ParseExecutionInputs(raw []byte, runID, gaggle string) (ExecutionInputs, error) {
	var in ExecutionInputs
	if len(raw) > MaxContextBytes {
		return in, errors.New("session context exceeds limit")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil {
		return in, err
	}
	if d.Decode(new(any)) != io.EOF {
		return in, errors.New("session context has trailing content")
	}
	canonical, err := in.Validate(runID, gaggle)
	if err != nil {
		return in, err
	}
	if !bytes.Equal(raw, canonical) {
		return in, errors.New("session context is not canonical")
	}
	return in, nil
}

func validStartDigests(e StartEnvelope) error {
	for _, v := range []string{e.ConfigGeneration, e.GooberDigest, e.MessageDigest, e.AuthorityDigest} {
		if len(v) != 71 || !strings.HasPrefix(v, "sha256:") || strings.Trim(v[7:], "0123456789abcdef") != "" {
			return errors.New("session execution digest is invalid")
		}
	}

	return nil
}

func validContextMessage(m Message, session string) bool {
	if m.SessionID != session || m.ID == "" || m.Sequence == 0 || len(m.Text) > MaxTextBytes || ValidatePRRepairTarget(m.RepairTarget) != nil {
		return false
	}
	switch m.ActorKind {
	case "human":
		return m.Actor != nil && m.Actor.Issuer != "" && m.Actor.Subject != ""
	case "agent", "system":
		return m.Actor == nil && m.RepairTarget == nil
	default:
		return false
	}
}
