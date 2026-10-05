package apicontract

import "github.com/goobers/goobers/internal/sessioning"

// UnmarshalJSON keeps human-selected repair authority free of ambiguous nested
// fields. Idempotency and verified human identity remain HTTP metadata.
func (r *SessionMessageRequest) UnmarshalJSON(raw []byte) error {
	input, err := sessioning.DecodeMessageContent(raw)
	if err != nil {
		return err
	}
	r.Text, r.RepairTarget = input.Text, input.RepairTarget
	return nil
}
