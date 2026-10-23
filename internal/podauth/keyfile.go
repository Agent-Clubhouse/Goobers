package podauth

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
)

// CredentialGrantKeyDomain separates derived grant-signing material from the
// raw pod-token signing key.
const CredentialGrantKeyDomain = "goobers/credential-refresh-grant/v1"

// CredentialGrantSigningKey returns a process-local random key when path is
// empty, or a domain-separated key derived from the shared key file.
func CredentialGrantSigningKey(path string) (*SignedKey, error) {
	if strings.TrimSpace(path) == "" {
		raw := make([]byte, sha256.Size)
		if _, err := rand.Read(raw); err != nil {
			return nil, fmt.Errorf("podauth: generate credential grant key: %w", err)
		}
		return NewSignedKey(raw)
	}
	return LoadDerivedSignedKeyFile(path, CredentialGrantKeyDomain)
}

// LoadSignedKeyFile loads trimmed signing material from path.
func LoadSignedKeyFile(path string) (*SignedKey, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("podauth: signing key file is required")
	}
	return loadKeyRingFile(path, "")
}

// LoadDerivedSignedKeyFile derives a signer from the key file under domain.
func LoadDerivedSignedKeyFile(path, domain string) (*SignedKey, error) {
	if strings.TrimSpace(domain) == "" {
		return nil, errors.New("podauth: derived signing key domain is required")
	}
	return loadKeyRingFile(path, domain)
}
