package podauth

import (
	"crypto/rand"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

// NewLocalStartAuthority creates process-local start proofs. It exposes no
// token minting or HTTP authentication capability, and cannot verify proofs
// after restart. Local execution and its receipts remain unverified.
func NewLocalStartAuthority() (livejournal.ControllerStartAuthority, error) {
	key := make([]byte, MinSignedKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	signer, err := NewSignedKey(key)
	clear(key)
	if err != nil {
		return nil, err
	}
	return localStartAuthority{signer: signer}, nil
}

type localStartAuthority struct{ signer *SignedKey }

func (a localStartAuthority) SealControllerStart(runID, key string, ev journal.Event) string {
	return a.signer.SealControllerStart(runID, key, ev)
}

func (a localStartAuthority) VerifyControllerStart(runID, key string, ev journal.Event, proof string) bool {
	return a.signer.VerifyControllerStart(runID, key, ev, proof)
}
