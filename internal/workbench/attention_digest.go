package workbench

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// NeedsHumanObservationDigest fixes one mechanically inspected source snapshot.
// It is evidence identity, not a bearer credential or permission to resolve it.
func NeedsHumanObservationDigest(observation NeedsHumanObservation) (string, error) {
	observation.Digest = ""
	raw, err := json.Marshal(observation)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}
