package podauth

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// SigningKey is one version of shared signing material. JSON encodes Key as base64.
type SigningKey struct {
	ID  string `json:"id"`
	Key []byte `json:"key"`
}

type signingKeys struct {
	current  SigningKey
	previous *SigningKey
}

// KeyReloadInterval bounds how long a cloud replica takes to observe a valid
// replacement file. Polling the path also handles Kubernetes Secret symlink swaps.
const KeyReloadInterval = 5 * time.Second
const maxKeyFileBytes = 64 * 1024

// NewKeyRing constructs an overlap-safe signer. Only current mints; both IDs
// verify. Untagged legacy tokens use current exclusively, never a key trial loop.
func NewKeyRing(current SigningKey, previous *SigningKey) (*SignedKey, error) {
	if err := validateSigningKey(current); err != nil {
		return nil, err
	}
	if previous != nil {
		if err := validateSigningKey(*previous); err != nil {
			return nil, err
		}
		if current.ID == previous.ID {
			return nil, errors.New("podauth: signing key IDs must be distinct")
		}
	}
	s, err := NewSignedKey(current.Key)
	if err != nil {
		return nil, err
	}
	keys := &signingKeys{current: SigningKey{ID: current.ID, Key: bytes.Clone(current.Key)}}
	if previous != nil {
		keys.previous = &SigningKey{ID: previous.ID, Key: bytes.Clone(previous.Key)}
	}
	s.keys.Store(keys)
	return s, nil
}

func validateSigningKey(key SigningKey) error {
	if key.ID == "" || len(key.ID) > 64 || strings.ContainsAny(key.ID, ".:") || strings.IndexFunc(key.ID, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-'
	}) >= 0 {
		return errors.New("podauth: key ID must contain 1..64 URL-safe letters, digits, underscores or hyphens")
	}
	if len(key.Key) < MinSignedKeyBytes {
		return fmt.Errorf("podauth: signing key must be at least %d bytes", MinSignedKeyBytes)
	}
	return nil
}

func (s *SignedKey) encodeToken(prefix, domain, payload string) string {
	keys := s.keys.Load() // One immutable snapshot covers the ID and MAC.
	if keys.current.ID != "" {
		payload = "kid:" + keys.current.ID + "." + payload
	}
	return prefix + payload + "." + signWith(keys.current.Key, domain+payload)
}

func (s *SignedKey) verificationKey(payload string) ([]byte, string, error) {
	keys := s.keys.Load()
	if !strings.HasPrefix(payload, "kid:") {
		return keys.current.Key, payload, nil
	}
	id, claims, ok := strings.Cut(strings.TrimPrefix(payload, "kid:"), ".")
	if !ok || id == "" || claims == "" {
		return nil, "", ErrMalformedToken
	}
	switch {
	case id == keys.current.ID:
		return keys.current.Key, claims, nil
	case keys.previous != nil && id == keys.previous.ID:
		return keys.previous.Key, claims, nil
	default:
		return nil, "", ErrUnknownToken
	}
}

func loadKeyRingFile(path, domain string) (*SignedKey, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("podauth: signing key file is required")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("podauth: open signing key file: %w", err)
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, maxKeyFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("podauth: read signing key file: %w", err)
	}
	if len(raw) > maxKeyFileBytes {
		return nil, errors.New("podauth: signing key file exceeds 64 KiB")
	}
	s, err := parseKeyRing(bytes.TrimSpace(raw))
	if err != nil {
		return nil, err
	}
	if domain != "" {
		keys := s.keys.Load()
		if keys.current.Key, err = deriveSigningKey(keys.current.Key, domain); err != nil {
			return nil, err
		}
		if keys.previous != nil {
			if keys.previous.Key, err = deriveSigningKey(keys.previous.Key, domain); err != nil {
				return nil, err
			}
		}
	}
	s.keyFile, s.derivationDomain = path, domain
	return s, nil
}

func deriveSigningKey(key []byte, domain string) ([]byte, error) {
	return hkdf.Key(sha256.New, key, nil, domain, sha256.Size)
}

func parseKeyRing(raw []byte) (*SignedKey, error) {
	if !bytes.HasPrefix(raw, []byte("{")) {
		return NewSignedKey(raw)
	}
	var document struct {
		Current  SigningKey  `json:"current"`
		Previous *SigningKey `json:"previous,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, errors.New("podauth: invalid signing key ring JSON")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("podauth: trailing signing key ring data")
	}
	return NewKeyRing(document.Current, document.Previous)
}

// ReloadKeyFile atomically publishes a validated replacement. Invalid/missing
// files retain the last good snapshot; callers must surface the returned error.
func (s *SignedKey) ReloadKeyFile() error {
	if s.keyFile == "" {
		return errors.New("podauth: signer has no key file")
	}
	replacement, err := loadKeyRingFile(s.keyFile, s.derivationDomain)
	if err != nil {
		return err
	}
	s.keys.Store(replacement.keys.Load())
	return nil
}

// WatchKeyFile reloads until cancellation, reporting failures without discarding
// the last good keys. interval may shorten, but never exceed, the reload bound.
func (s *SignedKey) WatchKeyFile(ctx context.Context, interval time.Duration, report func(error)) error {
	if s.keyFile == "" || report == nil {
		return errors.New("podauth: key-file watcher requires a file and error reporter")
	}
	if interval <= 0 || interval > KeyReloadInterval {
		return errors.New("podauth: key-file reload interval must be positive and at most 5s")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := s.ReloadKeyFile(); err != nil {
				report(err)
			}
		}
	}
}
