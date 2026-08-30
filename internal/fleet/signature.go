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

func GenerateKey() (*ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("fleet: generate instance key: %w", err)
	}
	return key, nil
}

func MarshalPrivateKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("fleet: marshal instance private key: %w", err)
	}
	return der, nil
}

func ParsePrivateKey(der []byte) (*ecdsa.PrivateKey, error) {
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("fleet: parse instance private key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errorsNewInvalidKey()
	}
	return key, nil
}

func errorsNewInvalidKey() error {
	return fmt.Errorf("fleet: instance private key is not ECDSA P-256")
}

func PublicKeySPKI(key *ecdsa.PrivateKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", fmt.Errorf("fleet: marshal instance public key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(der), nil
}

func ChallengePayload(fleetID, registrationID string, generation int64, connectionID, nonce string) string {
	return fmt.Sprintf("goobers-fleet-v1\n%s\n%s\n%d\n%s\n%s",
		fleetID, registrationID, generation, connectionID, nonce)
}

func SignChallenge(key *ecdsa.PrivateKey, fleetID, registrationID string, generation int64, connectionID, nonce string) (string, error) {
	digest := sha256.Sum256([]byte(ChallengePayload(fleetID, registrationID, generation, connectionID, nonce)))
	signature, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		return "", fmt.Errorf("fleet: sign challenge: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(signature), nil
}

func VerifyChallenge(publicKey *ecdsa.PublicKey, signature, fleetID, registrationID string, generation int64, connectionID, nonce string) error {
	decoded, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return fmt.Errorf("fleet: decode challenge signature: %w", err)
	}
	digest := sha256.Sum256([]byte(ChallengePayload(fleetID, registrationID, generation, connectionID, nonce)))
	if !ecdsa.VerifyASN1(publicKey, digest[:], decoded) {
		return fmt.Errorf("fleet: invalid challenge signature")
	}
	return nil
}
