package podauth

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
)

// ChildWorkflowGrantPrefix and the MAC domain are disjoint from pod, worker,
// and credential-refresh tokens. A run-wide pod token never gains this grant.
const ChildWorkflowGrantPrefix = "goobers-child."
const childWorkflowMACDomain = "goobers/child-workflow/v1\x00"

// MaxChildWorkflowGrantTTL bounds a stage credential independently of logical waits.
const MaxChildWorkflowGrantTTL = 24 * time.Hour
const maxChildWorkflowGrantBytes = 4096

// ErrInvalidChildWorkflowGrant reports an unauthenticated or malformed stage grant.
var ErrInvalidChildWorkflowGrant = errors.New("podauth: invalid child-workflow grant")

// ErrExpiredChildWorkflowGrant requires the launcher to rebind an active attempt.
var ErrExpiredChildWorkflowGrant = errors.New("podauth: child-workflow grant expired")

// ChildWorkflowGrant identifies one authenticated parent attempt. Occurrence
// includes the branch and loop visit and survives replacement attempts. ID is a
// fresh per-mint nonce: the service must check its live attempt/policy binding
// before every action, including reads. A valid signature alone is not admission.
// The grant carries no provider credential or user-authored policy.
type ChildWorkflowGrant struct {
	ID              string    `json:"id"`
	Gaggle          string    `json:"gaggle"`
	RunID           string    `json:"run"`
	StageOccurrence string    `json:"occurrence"`
	AttemptID       string    `json:"attempt"`
	ConfigDigest    string    `json:"configDigest"`
	PolicyDigest    string    `json:"policyDigest"`
	ExpiresAt       time.Time `json:"-"`
}

type childWorkflowPayload struct {
	ChildWorkflowGrant
	Exp int64 `json:"exp"`
}

// MintChildWorkflowGrant is for the trusted stage launcher after admission.
// Logical child waits may outlive a grant; the launcher must renew/rebind an
// active attempt before resuming agent work, rather than mint an unbounded token.
func (s *SignedKey) MintChildWorkflowGrant(grant ChildWorkflowGrant, ttl time.Duration) (string, ChildWorkflowGrant, error) {
	if !validChildWorkflowClaims(grant) || ttl <= 0 || ttl > MaxChildWorkflowGrantTTL {
		return "", ChildWorkflowGrant{}, ErrInvalidChildWorkflowGrant
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", ChildWorkflowGrant{}, fmt.Errorf("podauth: child grant nonce: %w", err)
	}
	grant.ID = base64.RawURLEncoding.EncodeToString(nonce)
	grant.ExpiresAt = s.now().Add(ttl).UTC().Truncate(time.Second)
	if !grant.ExpiresAt.After(s.now()) {
		return "", ChildWorkflowGrant{}, ErrInvalidChildWorkflowGrant
	}
	raw, err := json.Marshal(childWorkflowPayload{ChildWorkflowGrant: grant, Exp: grant.ExpiresAt.Unix()})
	if err != nil {
		return "", ChildWorkflowGrant{}, err
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return ChildWorkflowGrantPrefix + payload + "." + s.sign(childWorkflowMACDomain+payload), grant, nil
}

// VerifyChildWorkflowGrant authenticates bounded, closed claims. It does not
// check the live occurrence lease, cancellation fence or current authorization.
func (s *SignedKey) VerifyChildWorkflowGrant(token string) (ChildWorkflowGrant, error) {
	rest, ok := strings.CutPrefix(token, ChildWorkflowGrantPrefix)
	if !ok || len(token) > maxChildWorkflowGrantBytes {
		return ChildWorkflowGrant{}, ErrInvalidChildWorkflowGrant
	}
	payload, mac, ok := strings.Cut(rest, ".")
	if !ok || payload == "" || mac == "" || strings.Contains(mac, ".") ||
		subtle.ConstantTimeCompare([]byte(mac), []byte(s.sign(childWorkflowMACDomain+payload))) != 1 {
		return ChildWorkflowGrant{}, ErrInvalidChildWorkflowGrant
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return ChildWorkflowGrant{}, ErrInvalidChildWorkflowGrant
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var decoded childWorkflowPayload
	if err := decoder.Decode(&decoded); err != nil {
		return ChildWorkflowGrant{}, ErrInvalidChildWorkflowGrant
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return ChildWorkflowGrant{}, ErrInvalidChildWorkflowGrant
	}
	grant := decoded.ChildWorkflowGrant
	if !validChildWorkflowClaims(grant) || !validChildWorkflowText(grant.ID, 64) || decoded.Exp <= 0 {
		return ChildWorkflowGrant{}, ErrInvalidChildWorkflowGrant
	}
	grant.ExpiresAt = time.Unix(decoded.Exp, 0).UTC()
	now := s.now()
	if !grant.ExpiresAt.After(now) {
		return ChildWorkflowGrant{}, ErrExpiredChildWorkflowGrant
	}
	if grant.ExpiresAt.After(now.Add(MaxChildWorkflowGrantTTL)) {
		return ChildWorkflowGrant{}, ErrInvalidChildWorkflowGrant
	}
	return grant, nil
}

func validChildWorkflowClaims(grant ChildWorkflowGrant) bool {
	return validChildWorkflowText(grant.Gaggle, 128) && validChildWorkflowRunID(grant.RunID) &&
		validChildWorkflowText(grant.StageOccurrence, 256) && validChildWorkflowText(grant.AttemptID, 128) &&
		validChildWorkflowDigest(grant.ConfigDigest) && validChildWorkflowDigest(grant.PolicyDigest)
}

func validChildWorkflowRunID(id string) bool {
	return validChildWorkflowText(id, 256) && apiv1.ValidRunID(id) && url.PathEscape(id) == id
}

func validChildWorkflowText(s string, limit int) bool {
	if s == "" || len(s) > limit || !utf8.ValidString(s) || strings.TrimSpace(s) != s {
		return false
	}
	return !strings.ContainsFunc(s, unicode.IsControl)
}

func validChildWorkflowDigest(s string) bool {
	if len(s) != 71 || !strings.HasPrefix(s, "sha256:") {
		return false
	}
	for _, r := range s[7:] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// WithChildWorkflowGrants enables stage grants independently of the pod verifier,
// including when ordinary pod tokens use a daemon-local registry.
func (a *Authenticator) WithChildWorkflowGrants(key *SignedKey) *Authenticator {
	a.childGrants = key
	return a
}

func (a *Authenticator) authenticateChildWorkflow(token string) (*httpapi.Principal, error) {
	if a.childGrants == nil {
		return nil, ErrInvalidChildWorkflowGrant
	}
	grant, err := a.childGrants.VerifyChildWorkflowGrant(token)
	if err != nil {
		return nil, err
	}
	return &httpapi.Principal{
		Subject: "run:" + grant.RunID, Issuer: httpapi.ChildWorkflowPrincipalIssuer,
		ChildWorkflow: &httpapi.ChildWorkflowPrincipal{
			GrantID: grant.ID, Gaggle: grant.Gaggle, RunID: grant.RunID,
			StageOccurrence: grant.StageOccurrence, AttemptID: grant.AttemptID,
			ConfigDigest: grant.ConfigDigest, PolicyDigest: grant.PolicyDigest,
		},
	}, nil
}
