package journal

import (
	"errors"
	"strings"
	"unicode/utf8"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// ChildLineage identifies the exact durable child invocation and its retained
// proposal. SourceDigest is resolved only with this complete gaggle/parent/
// occurrence/key tuple in triggerqueue; a digest alone does not grant access.
// EnvelopeDigest commits to every pinned start input, including policy and
// placement, without copying credentials or authored source into run.yaml.
type ChildLineage struct {
	Gaggle          string `json:"gaggle"`
	ParentRunID     string `json:"parentRunId"`
	StageOccurrence string `json:"stageOccurrence"`
	InvocationKey   string `json:"invocationKey"`
	AcceptanceID    string `json:"acceptanceId"`
	SourceDigest    string `json:"sourceDigest"`
	EnvelopeDigest  string `json:"envelopeDigest"`
}

// ValidateChildLineage refuses malformed or cross-gaggle generated provenance.
// Legacy identities without child provenance retain their previous behavior.
// Validation does not authorize execution; callers must verify queue custody,
// pinned definitions, live permissions and workspace/scheduler admission.
func (id RunIdentity) ValidateChildLineage() error {
	c := id.Child
	if c == nil {
		return nil
	}
	valid := c.Gaggle == id.Gaggle && childIdentityText(c.Gaggle, 128) &&
		apiv1.ValidRunID(c.ParentRunID) && c.ParentRunID != id.RunID &&
		apiv1.ValidRunID(id.RunID) && c.AcceptanceID == "trigger-"+id.RunID &&
		childIdentityText(c.StageOccurrence, 256) && childIdentityText(c.InvocationKey, 256) &&
		id.ContinuedFromRunID == ""
	if !valid {
		return errors.New("journal: invalid child lineage")
	}
	for _, value := range []string{c.SourceDigest, c.EnvelopeDigest, id.ConfigGeneration, id.WorkflowDigest, id.GooberDigest} {
		if _, err := digestHex(value); err != nil || strings.ToLower(value) != value {
			return errors.New("journal: invalid child execution digest")
		}
	}
	return nil
}

func childIdentityText(value string, max int) bool {
	if value == "" || len(value) > max || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
