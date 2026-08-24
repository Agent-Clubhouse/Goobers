package podauth

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func testKey(b byte) []byte {
	key := make([]byte, MinSignedKeyBytes)
	for i := range key {
		key[i] = b + byte(i)
	}
	return key
}

func TestSignedKeyMintVerifyRoundTrip(t *testing.T) {
	s, err := NewSignedKey(testKey(1))
	if err != nil {
		t.Fatalf("NewSignedKey: %v", err)
	}
	token, err := s.Mint("run-abc", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !isPodToken(token) {
		t.Fatalf("minted token %q does not carry the pod prefix, so the authenticator would route it to the human fallback", token)
	}
	runID, err := s.verifySigned(token)
	if err != nil {
		t.Fatalf("verifySigned: %v", err)
	}
	if runID != "run-abc" {
		t.Fatalf("run ID = %q, want run-abc", runID)
	}
}

// The whole point of the signed mode: a DIFFERENT process holding the same key
// verifies a token it never minted. This is what the in-memory registry cannot
// do and why a split daemon/dispatcher deployment needs it (#3701).
func TestSignedKeyVerifiesTokenMintedByAnotherInstance(t *testing.T) {
	minter, err := NewSignedKey(testKey(7))
	if err != nil {
		t.Fatalf("NewSignedKey: %v", err)
	}
	verifier, err := NewSignedKey(testKey(7))
	if err != nil {
		t.Fatalf("NewSignedKey: %v", err)
	}
	token, err := minter.Mint("run-split", 0)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	runID, err := verifier.verifySigned(token)
	if err != nil {
		t.Fatalf("a separately constructed verifier holding the same key must accept the token: %v", err)
	}
	if runID != "run-split" {
		t.Fatalf("run ID = %q, want run-split", runID)
	}
}

func TestSignedKeyRejectsForeignKeyExpiryAndTampering(t *testing.T) {
	s, err := NewSignedKey(testKey(2))
	if err != nil {
		t.Fatalf("NewSignedKey: %v", err)
	}
	other, err := NewSignedKey(testKey(9))
	if err != nil {
		t.Fatalf("NewSignedKey: %v", err)
	}
	token, err := s.Mint("run-1", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	if _, err := other.verifySigned(token); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("a token signed by a different key must be rejected, got %v", err)
	}

	// Expiry, via the clock seam rather than a sleep.
	expired, err := (&SignedKey{key: testKey(2), now: func() time.Time { return time.Now().Add(-2 * time.Hour) }}).Mint("run-1", time.Hour)
	if err != nil {
		t.Fatalf("Mint (expired): %v", err)
	}
	if _, err := s.verifySigned(expired); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("an expired token must be rejected, got %v", err)
	}

	// Tampering with the payload must not survive the MAC.
	idx := strings.LastIndex(token, ".")
	forged := "goobers-pod." + "cnVuLTI" + token[idx:]
	if _, err := s.verifySigned(forged); err == nil {
		t.Fatal("a token whose run ID was swapped must not verify")
	}
}

func TestSignedKeyRefusesWeakKeyLongTTLAndAmbiguousRunID(t *testing.T) {
	if _, err := NewSignedKey([]byte("too-short")); err == nil {
		t.Fatal("a key shorter than the minimum must be refused at construction")
	}
	s, err := NewSignedKey(testKey(3))
	if err != nil {
		t.Fatalf("NewSignedKey: %v", err)
	}
	if _, err := s.Mint("run-1", MaxSignedTokenTTL+time.Minute); err == nil {
		t.Fatal("a TTL beyond the ceiling must be refused: signed tokens cannot be revoked")
	}
	// A dot would make the token encoding ambiguous; minting must refuse
	// rather than produce something that verifies as a different run.
	if _, err := s.Mint("run.with.dots", time.Hour); err == nil {
		t.Fatal("a run ID containing the delimiter must be refused")
	}
}
