package journal

import (
	"errors"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// SessionLineage identifies a real accepted conversation turn. It does not
// borrow an automation workflow, source run or continuation's authority.
type SessionLineage struct {
	Gaggle         string `json:"gaggle"`
	SessionID      string `json:"sessionId"`
	TurnID         string `json:"turnId"`
	MessageID      string `json:"messageId"`
	AcceptanceID   string `json:"acceptanceId"`
	EnvelopeDigest string `json:"envelopeDigest"`
	InputDigest    string `json:"inputDigest"`
}

// ValidateSessionLineage validates provenance, not current execution permission.
func (id RunIdentity) ValidateSessionLineage() error {
	s := id.Session
	if s == nil {
		return nil
	}
	if id.Child != nil || id.Event != nil || id.ContinuedFromRunID != "" || !apiv1.ValidRunID(id.RunID) || s.AcceptanceID != "trigger-"+id.RunID || s.Gaggle != id.Gaggle || id.Trigger.Kind != TriggerSignal || id.Trigger.Ref != "session:"+s.SessionID+":"+s.TurnID {
		return errors.New("journal: invalid session lineage")
	}
	for _, value := range []string{s.Gaggle, s.SessionID, s.TurnID, s.MessageID} {
		if !childIdentityText(value, 128) {
			return errors.New("journal: invalid session identity")
		}
	}
	for _, value := range []string{s.EnvelopeDigest, s.InputDigest, id.ConfigGeneration, id.WorkflowDigest, id.GooberDigest} {
		if _, err := digestHex(value); err != nil || strings.ToLower(value) != value {
			return errors.New("journal: invalid session execution digest")
		}
	}
	return nil
}
