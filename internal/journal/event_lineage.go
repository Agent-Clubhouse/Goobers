package journal

import (
	"errors"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// EventLineage identifies one accepted consumer group. It is host provenance;
// event payloads cannot select this identity or grant a workflow authority.
type EventLineage struct {
	Gaggle         string `json:"gaggle"`
	GroupID        string `json:"groupId"`
	Consumer       string `json:"consumer"`
	Revision       string `json:"revision"`
	AcceptanceID   string `json:"acceptanceId"`
	EnvelopeDigest string `json:"envelopeDigest"`
	ManifestDigest string `json:"manifestDigest"`
}

// ValidateEventLineage refuses mixed or incomplete routed-run provenance.
// Human continuations clear this execution identity and retain copied inputs.
func (id RunIdentity) ValidateEventLineage() error {
	e := id.Event
	if e == nil {
		return nil
	}
	if id.Child != nil || id.ContinuedFromRunID != "" || !apiv1.ValidRunID(id.RunID) || e.AcceptanceID != "trigger-"+id.RunID || e.Gaggle != id.Gaggle || id.Trigger.Kind != TriggerSignal || id.Trigger.Ref != "event:"+e.GroupID {
		return errors.New("journal: invalid event execution lineage")
	}
	for _, value := range []string{e.Gaggle, e.GroupID, e.Consumer, e.Revision} {
		if !childIdentityText(value, 256) {
			return errors.New("journal: invalid event identity")
		}
	}
	for _, value := range []string{e.EnvelopeDigest, e.ManifestDigest, id.ConfigGeneration, id.WorkflowDigest, id.GooberDigest} {
		if _, err := digestHex(value); err != nil || strings.ToLower(value) != value {
			return errors.New("journal: invalid event execution digest")
		}
	}
	return nil
}

func (id RunIdentity) validateExecutionLineage() error {
	if err := id.ValidateChildLineage(); err != nil {
		return err
	}
	return id.ValidateEventLineage()
}
