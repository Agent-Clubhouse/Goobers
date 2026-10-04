package eventing

import (
	"encoding/json"
	"errors"
)

// StartKind identifies a pinned event consumer start. It must never be
// interpreted as an ordinary named-catalog trigger by a legacy dispatcher.
const StartKind = "goobers.event-start/v1"

// StartEnvelope carries only bounded group references. Consumer inputs are
// hydrated through group membership, so all mode never inlines 1,000 payloads.
type StartEnvelope struct {
	Kind             string `json:"kind"`
	Gaggle           string `json:"gaggle"`
	GroupID          string `json:"groupId"`
	Consumer         string `json:"consumer"`
	Revision         string `json:"revision"`
	Workflow         string `json:"workflow"`
	WorkflowDigest   string `json:"workflowDigest"`
	GooberDigest     string `json:"gooberDigest"`
	ConfigGeneration string `json:"configGeneration"`
	InputMode        string `json:"inputMode"`
	EventCount       int    `json:"eventCount"`
	SelectedReceipt  string `json:"selectedReceipt,omitempty"`
}

// Marshal validates the typed immutable event-to-start handoff.
func (s StartEnvelope) Marshal() ([]byte, error) {
	if s.Kind != StartKind || s.EventCount < 1 || s.EventCount > 1000 || (s.InputMode != "all" && s.InputMode != "latest") {
		return nil, errors.New("eventing: invalid start envelope")
	}
	for _, value := range []string{s.Gaggle, s.GroupID, s.Consumer, s.Revision, s.Workflow, s.WorkflowDigest, s.GooberDigest, s.ConfigGeneration} {
		if !boundedText(value, 256) {
			return nil, errors.New("eventing: incomplete start identity")
		}
	}
	if (s.InputMode == "latest" && !boundedText(s.SelectedReceipt, 256)) || (s.InputMode == "all" && s.SelectedReceipt != "") {
		return nil, errors.New("eventing: invalid selected receipt")
	}
	return json.Marshal(s)
}

// ParseStart refuses unknown or mutable authority in a queued event start.
func ParseStart(raw []byte) (StartEnvelope, error) {
	var result StartEnvelope
	if err := decodeClosed(raw, 16<<10, &result); err != nil {
		return result, err
	}
	_, err := result.Marshal()
	return result, err
}
