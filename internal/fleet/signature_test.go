package fleet

import (
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

func TestChallengePayloadInteroperabilityVector(t *testing.T) {
	const want = "goobers-fleet-v1\nfleet-1\nregistration-2\n7\nconnection-3\nnonce-4"
	got := ChallengePayload("fleet-1", "registration-2", 7, "connection-3", "nonce-4")
	if got != want {
		t.Fatalf("payload = %q, want %q", got, want)
	}
	sum := sha256.Sum256([]byte(got))
	if encoded := base64.RawURLEncoding.EncodeToString(sum[:]); encoded != "u-95US6XuMqQHDR-5yiJs4ZshI80iZQGjV0HakbueOE" {
		t.Fatalf("payload digest = %q", encoded)
	}
}

func TestSignChallengeUsesP256ASN1Base64URL(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	signature, err := SignChallenge(key, "fleet", "registration", 4, "connection", "nonce")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := base64.RawURLEncoding.DecodeString(signature); err != nil {
		t.Fatalf("signature is not unpadded base64url: %v", err)
	}
	if err := VerifyChallenge(&key.PublicKey, signature, "fleet", "registration", 4, "connection", "nonce"); err != nil {
		t.Fatal(err)
	}
	if err := VerifyChallenge(&key.PublicKey, signature, "fleet", "registration", 5, "connection", "nonce"); err == nil {
		t.Fatal("signature verified for the wrong generation")
	}
}
