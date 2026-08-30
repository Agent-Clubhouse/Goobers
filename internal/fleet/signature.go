package fleet

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"fmt"
)

// GenerateKey generates a new ECDSA P-256 instance key.
func GenerateKey() (*ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("fleet: generate instance key: %w", err)
	}
	return key, nil
}

// MarshalPrivateKey encodes key as a PKCS#8 DER-encoded private key.
func MarshalPrivateKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("fleet: marshal instance private key: %w", err)
	}
	return der, nil
}

// ParsePrivateKey decodes a PKCS#8 DER-encoded ECDSA P-256 private key.
func ParsePrivateKey(der []byte) (*ecdsa.PrivateKey, error) {
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("fleet: parse instance private key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("fleet: instance private key is not ECDSA P-256")
	}
	return key, nil
}

// PublicKeySPKI returns the base64-encoded SubjectPublicKeyInfo for key's
// public key.
func PublicKeySPKI(key *ecdsa.PrivateKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", fmt.Errorf("fleet: marshal instance public key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(der), nil
}

// ChallengePayload returns the canonical byte payload used to sign and
// verify a connection challenge.
func ChallengePayload(fleetID, registrationID string, generation int64, connectionID, nonce string) string {
	return fmt.Sprintf("goobers-fleet-v1\n%s\n%s\n%d\n%s\n%s",
		fleetID, registrationID, generation, connectionID, nonce)
}

// SignChallenge signs the connection challenge payload with key and returns
// the base64url-encoded ASN.1 signature.
func SignChallenge(key *ecdsa.PrivateKey, fleetID, registrationID string, generation int64, connectionID, nonce string) (string, error) {
	digest := sha256.Sum256([]byte(ChallengePayload(fleetID, registrationID, generation, connectionID, nonce)))
	signature, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		return "", fmt.Errorf("fleet: sign challenge: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(signature), nil
}
