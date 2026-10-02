package journal

import (
	"encoding/json"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// UnmarshalJSON preserves legacy event decoding while refusing malformed
// revision controls before their presence or spelling can be lost.
func (e *Event) UnmarshalJSON(data []byte) error {
	if _, err := apiv1.DecodeWorkspaceRevisionField(data); err != nil {
		return err
	}
	type event Event
	return json.Unmarshal(data, (*event)(e))
}
