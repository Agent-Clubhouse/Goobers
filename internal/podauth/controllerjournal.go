package podauth

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/launchreceipt"
	"github.com/goobers/goobers/internal/livejournal"
)

const controllerJournalDomain = "goobers/controller-journal/v1\x00"
const controllerStartProofDomain = "goobers/controller-start-proof/v1\x00"

type controllerJournalClaims struct {
	RunID   string `json:"run"`
	Digest  string `json:"digest"`
	Expires int64  `json:"expires"`
}

// MintControllerJournal grants only emission of one run's exact batch.
func (s *SignedKey) MintControllerJournal(runID, digest string, ttl time.Duration) (string, error) {
	if !apiv1.ValidRunID(runID) || len(runID) > 256 || !launchreceipt.ValidDigest(digest) || ttl <= 0 || ttl > livejournal.ControllerJournalTTL {
		return "", launchreceipt.ErrInvalid
	}
	raw, err := json.Marshal(controllerJournalClaims{RunID: runID, Digest: digest, Expires: s.now().Add(ttl).Unix()})
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return livejournal.ControllerJournalTokenPrefix + payload + "." + s.sign(controllerJournalDomain+payload), nil
}

// VerifyControllerJournal checks distinct authority, exact claims, and expiry.
func (s *SignedKey) VerifyControllerJournal(token string) (string, string, error) {
	rest, ok := strings.CutPrefix(token, livejournal.ControllerJournalTokenPrefix)
	if !ok || len(token) > 2048 {
		return "", "", launchreceipt.ErrInvalid
	}
	payload, mac, ok := strings.Cut(rest, ".")
	if !ok || subtle.ConstantTimeCompare([]byte(mac), []byte(s.sign(controllerJournalDomain+payload))) != 1 {
		return "", "", launchreceipt.ErrInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", "", launchreceipt.ErrInvalid
	}
	var claims controllerJournalClaims
	if json.Unmarshal(raw, &claims) != nil || !apiv1.ValidRunID(claims.RunID) || len(claims.RunID) > 256 || !launchreceipt.ValidDigest(claims.Digest) || claims.Expires <= s.now().Unix() || claims.Expires > s.now().Add(livejournal.ControllerJournalTTL).Unix() {
		return "", "", launchreceipt.ErrInvalid
	}
	return claims.RunID, claims.Digest, nil
}

// SealControllerStart authenticates immutable anchor metadata without minting
// reusable HTTP authority. The proof remains verifiable after daemon restart.
func (s *SignedKey) SealControllerStart(runID, key string, ev journal.Event) string {
	metadata := struct {
		RunID   string
		Key     string
		Seq     uint64
		Type    journal.EventType
		Stage   string
		Branch  int
		Attempt int
		Class   journal.AttemptClass
	}{runID, key, ev.Seq, ev.Type, ev.Stage, ev.Branch, ev.Attempt, ev.AttemptClass}
	raw, _ := json.Marshal(metadata) // Fixed scalar fields cannot fail encoding.
	return s.sign(controllerStartProofDomain + base64.RawURLEncoding.EncodeToString(raw))
}

// VerifyControllerStart rejects copied, altered, or unsigned origin markers.
func (s *SignedKey) VerifyControllerStart(runID, key string, ev journal.Event, proof string) bool {
	return subtle.ConstantTimeCompare([]byte(proof), []byte(s.SealControllerStart(runID, key, ev))) == 1
}

func (a *Authenticator) authenticateControllerJournal(token string) (*httpapi.Principal, error) {
	verifier, ok := a.verifier.(livejournal.ControllerJournalVerifier)
	if !ok {
		return nil, launchreceipt.ErrInvalid
	}
	runID, _, err := verifier.VerifyControllerJournal(token)
	if err != nil {
		return nil, err
	}
	return &httpapi.Principal{Subject: "run:" + runID, Issuer: httpapi.ControllerJournalPrincipalIssuer}, nil
}
