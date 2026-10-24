package podauth

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/httpapi"
)

const (
	workerTokenPrefix          = "goobers-worker."
	workerBlobTokenPrefix      = "goobers-worker-blob."
	workerSurrenderTokenPrefix = "goobers-worker-surrender-read."
	workerChildTokenPrefix     = "goobers-worker-child-admission."
)

// workerTokenKind parameterises one worker credential. The MAC domain is
// non-wire and cannot be a valid pod payload: its NUL delimiter is forbidden by
// the pod token's base64/decimal grammar. The wire prefix alone is insufficient
// because it can itself be parsed as a base64 pod segment.
type workerTokenKind struct {
	prefix    string
	macDomain string
	issuer    string
}

var (
	workerConfigDigestKind = workerTokenKind{workerTokenPrefix, "goobers/worker-config-digest/v1\x00", httpapi.WorkerPrincipalIssuer}
	workerBlobKind         = workerTokenKind{workerBlobTokenPrefix, "goobers/worker-blob/v1\x00", httpapi.WorkerBlobPrincipalIssuer}
	workerSurrenderKind    = workerTokenKind{workerSurrenderTokenPrefix, "goobers/worker-surrender-read/v1\x00", httpapi.WorkerSurrenderPrincipalIssuer}
	workerChildKind        = workerTokenKind{workerChildTokenPrefix, "goobers/worker-child-admission/v1\x00", httpapi.WorkerChildAdmissionPrincipalIssuer}
)

// workerTokenKinds lists longer prefixes before the config-digest prefix they extend.
var workerTokenKinds = []workerTokenKind{workerChildKind, workerSurrenderKind, workerBlobKind, workerConfigDigestKind}

const maxWorkerTokenBodyBytes = 512

// MaxWorkerTokenTTL bounds the config-observability worker identity. Workers
// mint a fresh bearer for each digest poll or transition report; this
// credential never enters a stage pod and reaches no run or config content.
const MaxWorkerTokenTTL = 5 * time.Minute

// MintWorkerConfigDigest authenticates the worker itself, without inventing a
// run identity. The separate wire prefix and MAC domain prevent converting this
// narrow config-observability credential into a run-scoped pod token by
// replacing its prefix.
func (s *SignedKey) MintWorkerConfigDigest(workerID string, ttl time.Duration) (string, error) {
	return s.mintWorkerToken(workerConfigDigestKind, workerID, ttl)
}

// MintWorkerBlob grants a resident worker access only to blob GET/PUT.
// Its wire prefix and MAC domain are separate from pod and config-digest tokens.
func (s *SignedKey) MintWorkerBlob(workerID string, ttl time.Duration) (string, error) {
	return s.mintWorkerToken(workerBlobKind, workerID, ttl)
}

// MintWorkerSurrender grants a resident worker access only to surrender GET/Seen.
// Its wire prefix and MAC domain are separate from pod and config-digest tokens.
func (s *SignedKey) MintWorkerSurrender(workerID string, ttl time.Duration) (string, error) {
	return s.mintWorkerToken(workerSurrenderKind, workerID, ttl)
}

// MintWorkerChildAdmission grants a resident engine worker access only to the
// engine child-admission plane. Its wire prefix and MAC domain are separate
// from every pod and other worker credential. The grant is instance-wide: it
// names the worker, not a run or child, so it reaches every generated child of
// the instance (see docs/design/worker-blob-transport.md).
func (s *SignedKey) MintWorkerChildAdmission(workerID string, ttl time.Duration) (string, error) {
	return s.mintWorkerToken(workerChildKind, workerID, ttl)
}

func (s *SignedKey) mintWorkerToken(kind workerTokenKind, workerID string, ttl time.Duration) (string, error) {
	if !validWorkerID(workerID) {
		return "", fmt.Errorf("podauth: a nonempty worker identity of at most 256 bytes without control characters is required")
	}
	if ttl <= 0 || ttl > MaxWorkerTokenTTL {
		return "", fmt.Errorf("podauth: worker token TTL must be positive and at most %s", MaxWorkerTokenTTL)
	}
	payload := base64.RawURLEncoding.EncodeToString([]byte(workerID)) + "." + strconv.FormatInt(s.now().Add(ttl).Unix(), 10)
	return s.encodeToken(kind.prefix, kind.macDomain, payload), nil
}

func validWorkerID(id string) bool {
	return id != "" && len(id) <= 256 && strings.TrimSpace(id) == id && utf8.ValidString(id) && strings.IndexFunc(id, unicode.IsControl) < 0
}

func (s *SignedKey) verifyWorkerToken(kind workerTokenKind, token string) (string, error) {
	payload, err := s.verifyMAC(token, kind.prefix, kind.macDomain)
	if err != nil {
		return "", err
	}
	segments := strings.Split(payload, ".")
	if len(segments) != 2 || len(token) > len(kind.prefix)+maxWorkerTokenBodyBytes {
		return "", ErrMalformedToken
	}
	identity, err := base64.RawURLEncoding.DecodeString(segments[0])
	if err != nil || !validWorkerID(string(identity)) {
		return "", ErrMalformedToken
	}
	exp, err := strconv.ParseInt(segments[1], 10, 64)
	if err != nil {
		return "", ErrMalformedToken
	}
	if !s.unexpired(exp) || time.Unix(exp, 0).After(s.now().Add(MaxWorkerTokenTTL)) {
		return "", ErrUnknownToken
	}
	return string(identity), nil
}

type workerVerifier interface {
	verifyWorkerToken(workerTokenKind, string) (string, error)
}

func (a *Authenticator) authenticateWorker(kind workerTokenKind, token string) (*httpapi.Principal, error) {
	return authenticateWith(a, func(v workerVerifier) (*httpapi.Principal, error) {
		id, err := v.verifyWorkerToken(kind, token)
		if err != nil {
			return nil, err
		}
		return &httpapi.Principal{Subject: "worker:" + id, Issuer: kind.issuer}, nil
	})
}
