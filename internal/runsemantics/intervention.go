package runsemantics

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// InterventionContent is the stable local/cloud idempotency payload. Field
// order is part of the existing local fingerprint contract.
type InterventionContent struct {
	Action              string `json:"action"`
	RunID               string `json:"runId"`
	Stage               string `json:"stage"`
	Actor               string `json:"actor"`
	Decision            string `json:"decision"`
	Rationale           string `json:"rationale"`
	InstructionAddendum string `json:"instructionAddendum"`
}

// InterventionFingerprint binds the complete domain request, independent of
// transport metadata, identically for the local and cloud journal producers.
func InterventionFingerprint(content InterventionContent) (string, error) {
	encoded, err := json.Marshal(content)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(encoded)), nil
}
