package eventing

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// KindPublishEvent selects the host-local typed deterministic built-in.
const KindPublishEvent = "publish-event"

// Publication is author data. Identity, source and ancestry remain host-owned.
type Publication struct {
	Type          string `json:"type"`
	Subject       string `json:"subject,omitempty"`
	Data          any    `json:"data,omitempty"`
	OccurrenceKey string `json:"occurrenceKey"`
}

// ParsePublication accepts only the bounded built-in fields. It never accepts
// a destination gaggle, root, actor, CloudEvents source or event ID override.
func ParsePublication(inputs map[string]any) (Publication, error) {
	var p Publication
	for key := range inputs {
		if !slices.Contains([]string{"kind", "type", "subject", "data", "occurrenceKey"}, key) {
			return p, errors.New("event publication contains an unsupported input")
		}
	}
	if inputs["kind"] != KindPublishEvent {
		return p, errors.New("event publication kind is required")
	}
	raw, err := json.Marshal(inputs)
	if err != nil || len(raw) > MaxEnvelopeBytes {
		return p, errors.New("event publication input exceeds bounds")
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		return p, err
	}
	delete(fields, "kind")
	raw, err = json.Marshal(fields)
	if err != nil {
		return p, err
	}
	if err = json.Unmarshal(raw, &p); err != nil {
		return p, errors.New("event publication has invalid input types")
	}
	if value, supplied := inputs["data"]; supplied {
		text, ok := value.(string)
		if !ok || !json.Valid([]byte(text)) {
			return p, errors.New("event publication data must be a JSON string")
		}
		p.Data = json.RawMessage(text)
	}
	if !boundedText(p.OccurrenceKey, 128) || !boundedText(p.Type, 256) {
		return p, errors.New("event publication requires bounded type and occurrenceKey")
	}
	return p, nil
}

// Envelope derives a stable source/ID from host-bound occurrence and author key.
// Time is omitted so a retry has exactly the same normalized envelope bytes.
func (p Publication) Envelope(gaggle, runID, occurrence string) (Envelope, error) {
	id := fmt.Sprintf("evt_%x", sha256.Sum256([]byte(gaggle+"\x00"+runID+"\x00"+occurrence+"\x00"+p.OccurrenceKey)))
	object := map[string]any{"specversion": "1.0", "id": id, "source": "urn:goobers:workflow:" + runID, "type": p.Type, "datacontenttype": "application/json"}
	if p.Subject != "" {
		object["subject"] = p.Subject
	}
	if p.Data != nil {
		object["data"] = p.Data
	}
	raw, err := json.Marshal(object)
	if err != nil {
		return Envelope{}, err
	}
	return Parse(raw)
}

// AllowsPublication checks exact workflow/type literals; no wildcard grants.
func AllowsPublication(policy *apiv1.GaggleEvents, workflow, eventType string) bool {
	if policy == nil {
		return false
	}
	for _, p := range policy.Publishers {
		if p.Workflow == workflow && slices.Contains(p.AllowedTypes, eventType) {
			return true
		}
	}
	return false
}

// ValidatePublishers rejects duplicate, empty and unbounded publication grants.
func ValidatePublishers(policy *apiv1.GaggleEvents) error {
	if policy == nil {
		return nil
	}
	if len(policy.Publishers) > MaxSubscriptions {
		return errors.New("eventing: too many publishers")
	}
	seen := map[string]bool{}
	for _, p := range policy.Publishers {
		if !boundedText(p.Workflow, 128) || seen[p.Workflow] || len(p.AllowedTypes) == 0 || len(p.AllowedTypes) > 32 {
			return errors.New("eventing: invalid or duplicate publisher")
		}
		seen[p.Workflow] = true
		types := map[string]bool{}
		for _, typ := range p.AllowedTypes {
			if !boundedText(typ, 256) || types[typ] {
				return errors.New("eventing: invalid or duplicate allowed type")
			}
			types[typ] = true
		}
	}
	return nil
}
