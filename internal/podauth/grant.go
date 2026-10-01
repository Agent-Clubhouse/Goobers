package podauth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// CredentialGrantPrefix routes a stage credential-refresh grant (Goobers#6120).
// It is distinct from the pod and worker prefixes and signed under its own MAC
// domain, so none of the three token kinds can be converted into another by
// editing its prefix.
const CredentialGrantPrefix = "goobers-grant."

// credentialGrantMACDomain separates grant signatures from pod and worker
// token signatures made with the same key. The NUL cannot occur in a grant's
// base64url payload.
const credentialGrantMACDomain = "goobers/credential-grant/v1\x00"

// MaxCredentialGrantTTL bounds a grant's life. A grant cannot be revoked
// before it expires except by restarting the daemon that holds its key, so
// the ceiling is the containment a leaked grant has. It sits above the
// longest shipped stage timeout (the large-repo preset's 4h) with room for a
// slow operator-set default; a stage that outlives it falls back to the
// pre-grant behavior (its last delivered value) for the remainder.
const MaxCredentialGrantTTL = 24 * time.Hour

// maxCredentialGrantBytes bounds a presented grant before any decoding.
const maxCredentialGrantBytes = 4096

// maxCredentialGrantCapabilities bounds the capability list a grant carries.
const maxCredentialGrantCapabilities = 32

// ErrInvalidCredentialGrant reports a grant that is malformed, forged, or
// signed by another key (a restarted daemon's grants all read this way).
var ErrInvalidCredentialGrant = errors.New("podauth: invalid credential grant")

// ErrExpiredCredentialGrant reports a well-signed grant past its expiry.
var ErrExpiredCredentialGrant = errors.New("podauth: credential grant expired")

// CredentialGrant is what a stage credential-refresh grant authorizes: the
// holder may re-resolve, one at a time, the named Capabilities of exactly one
// attempt of one deterministic stage of one run, until ExpiresAt. The claims
// are inside the signed payload, so a holder cannot widen any of them.
//
// ID is a random per-mint identifier: two executions of one stage in one run
// can carry the same RunID/Stage/Attempt (a workflow that revisits a stage
// restarts its attempt numbering), so revocation keys on ID, never on the
// attempt triple.
type CredentialGrant struct {
	ID           string    `json:"id"`
	RunID        string    `json:"run"`
	Stage        string    `json:"stage"`
	Attempt      int32     `json:"attempt"`
	Capabilities []string  `json:"caps"`
	ExpiresAt    time.Time `json:"-"`
}

type credentialGrantPayload struct {
	CredentialGrant
	Exp int64 `json:"exp"`
}

// Allows reports whether the grant names capability.
func (g CredentialGrant) Allows(capability string) bool {
	return capability != "" && slices.Contains(g.Capabilities, capability)
}

// IsCredentialGrant reports whether token carries the grant prefix. It says
// nothing about validity.
func IsCredentialGrant(token string) bool {
	return strings.HasPrefix(token, CredentialGrantPrefix)
}

// MintCredentialGrant signs grant for ttl (capped at MaxCredentialGrantTTL).
func (s *SignedKey) MintCredentialGrant(grant CredentialGrant, ttl time.Duration) (string, CredentialGrant, error) {
	if err := validateGrantClaims(grant); err != nil {
		return "", CredentialGrant{}, err
	}
	if ttl <= 0 {
		return "", CredentialGrant{}, fmt.Errorf("podauth: credential grant TTL must be positive, got %s", ttl)
	}
	ttl = min(ttl, MaxCredentialGrantTTL)
	grant.Capabilities = normalizeGrantCapabilities(grant.Capabilities)
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", CredentialGrant{}, fmt.Errorf("podauth: credential grant ID: %w", err)
	}
	grant.ID = base64.RawURLEncoding.EncodeToString(nonce)
	grant.ExpiresAt = s.now().Add(ttl).UTC().Truncate(time.Second)
	raw, err := json.Marshal(credentialGrantPayload{CredentialGrant: grant, Exp: grant.ExpiresAt.Unix()})
	if err != nil {
		return "", CredentialGrant{}, fmt.Errorf("podauth: encode credential grant: %w", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return CredentialGrantPrefix + payload + "." + s.sign(credentialGrantMACDomain+payload), grant, nil
}

// VerifyCredentialGrant checks token's signature and expiry and returns its
// claims.
func (s *SignedKey) VerifyCredentialGrant(token string) (CredentialGrant, error) {
	rest, ok := strings.CutPrefix(token, CredentialGrantPrefix)
	if !ok || len(token) > maxCredentialGrantBytes {
		return CredentialGrant{}, ErrInvalidCredentialGrant
	}
	payload, mac, ok := strings.Cut(rest, ".")
	if !ok || payload == "" || mac == "" || strings.Contains(mac, ".") {
		return CredentialGrant{}, ErrInvalidCredentialGrant
	}
	if subtle.ConstantTimeCompare([]byte(mac), []byte(s.sign(credentialGrantMACDomain+payload))) != 1 {
		return CredentialGrant{}, ErrInvalidCredentialGrant
	}
	grant, err := decodeGrantPayload(payload)
	if err != nil {
		return CredentialGrant{}, err
	}
	if !grant.ExpiresAt.After(s.now()) {
		return CredentialGrant{}, ErrExpiredCredentialGrant
	}
	return grant, nil
}

func decodeGrantPayload(payload string) (CredentialGrant, error) {
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return CredentialGrant{}, ErrInvalidCredentialGrant
	}
	var decoded credentialGrantPayload
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return CredentialGrant{}, ErrInvalidCredentialGrant
	}
	grant := decoded.CredentialGrant
	grant.ExpiresAt = time.Unix(decoded.Exp, 0).UTC()
	if validateGrantClaims(grant) != nil || decoded.Exp <= 0 || grant.ID == "" {
		return CredentialGrant{}, ErrInvalidCredentialGrant
	}
	return grant, nil
}

func validateGrantClaims(grant CredentialGrant) error {
	switch {
	case strings.TrimSpace(grant.RunID) == "":
		return errors.New("podauth: credential grant requires a run ID")
	case strings.TrimSpace(grant.Stage) == "":
		return errors.New("podauth: credential grant requires a stage")
	case grant.Attempt < 0:
		return errors.New("podauth: credential grant attempt must not be negative")
	case len(grant.Capabilities) == 0:
		// A grant for nothing is not a narrower grant; it is a mistake that
		// would otherwise surface far away as a refused refresh.
		return errors.New("podauth: credential grant requires at least one capability")
	case len(grant.Capabilities) > maxCredentialGrantCapabilities:
		return fmt.Errorf("podauth: credential grant names more than %d capabilities", maxCredentialGrantCapabilities)
	}
	for _, capability := range grant.Capabilities {
		if strings.TrimSpace(capability) == "" {
			return errors.New("podauth: credential grant names an empty capability")
		}
	}
	return nil
}

func normalizeGrantCapabilities(capabilities []string) []string {
	out := slices.Clone(capabilities)
	sort.Strings(out)
	return slices.Compact(out)
}
