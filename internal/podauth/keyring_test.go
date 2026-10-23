package podauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
)

func ringKey(id string, b byte) SigningKey {
	return SigningKey{ID: id, Key: bytes.Repeat([]byte{b}, 32)}
}
func writeRing(t *testing.T, path string, current SigningKey, previous *SigningKey) {
	t.Helper()
	raw, err := json.Marshal(struct {
		Current  SigningKey  `json:"current"`
		Previous *SigningKey `json:"previous,omitempty"`
	}{current, previous})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".next", raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".next", path); err != nil {
		t.Fatal(err)
	}
}
func mustRing(t *testing.T, current SigningKey, previous *SigningKey) *SignedKey {
	t.Helper()
	s, err := NewKeyRing(current, previous)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func authenticateRingToken(t *testing.T, s *SignedKey, token string) error {
	t.Helper()
	auth, err := NewAuthenticator(s, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	_, err = auth.WithCredentialGrants(s).WithChildWorkflowGrants(s).Authenticate(req)
	return err
}

func TestRotationDrillNo401(t *testing.T) {
	old, newKey := ringKey("old", 'o'), ringKey("new", 'n')
	path := filepath.Join(t.TempDir(), "keys.json")
	writeRing(t, path, old, nil)
	first, err := LoadSignedKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadSignedKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var retained []string
	// First distribute the new verifier, then promote the minting key. Reload one
	// replica at a time while tokens from every previous step remain usable.
	for _, step := range []struct {
		current  SigningKey
		previous *SigningKey
	}{{old, &newKey}, {newKey, &old}} {
		writeRing(t, path, step.current, step.previous)
		for _, replica := range []*SignedKey{first, second} {
			if err := replica.ReloadKeyFile(); err != nil {
				t.Fatal(err)
			}
			minted := mintRingTokenKinds(t, replica)
			retained = append(retained, minted...)
			for _, verifier := range []*SignedKey{first, second} {
				for _, token := range retained {
					if err := authenticateRingToken(t, verifier, token); err != nil {
						t.Fatalf("rotation rejected token: %v", err)
					}
				}
			}
		}
	}
	writeRing(t, path, newKey, nil)
	if err := second.ReloadKeyFile(); err != nil {
		t.Fatal(err)
	}
	for _, token := range retained {
		err := authenticateRingToken(t, second, token)
		if strings.Contains(token, "kid:old.") && err == nil {
			t.Fatal("retired key still admitted")
		}
		if strings.Contains(token, "kid:new.") && err != nil {
			t.Fatalf("current key rejected: %v", err)
		}
	}
}

func mintRingTokenKinds(t *testing.T, s *SignedKey) []string {
	t.Helper()
	var tokens []string
	add := func(token string, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, token)
	}
	add(s.Mint("run-1", time.Hour))
	add(s.MintScoped("run-1", time.Hour, ScopeClaims))
	add(s.MintWorkerConfigDigest("worker", time.Minute))
	add(s.MintWorkerBlob("worker", time.Minute))
	add(s.MintWorkerSurrender("worker", time.Minute))
	digest := "sha256:" + strings.Repeat("a", 64)
	add(s.MintChildPod("run-1", digest, time.Hour))
	add(s.MintWorkflowParentPod("run-1", digest, time.Hour))
	grant, _, err := s.MintCredentialGrant(testCredentialGrant(), time.Hour)
	add(grant, err)
	child, claims, err := s.MintChildWorkflowGrant(childGrantFixture(), time.Hour)
	add(child, err)
	add(s.RecoverChildWorkflowGrant(claims))
	return tokens
}

func TestTokenWithoutKidVerifies(t *testing.T) {
	current, previous := ringKey("current", 'c'), ringKey("previous", 'p')
	legacy, err := NewSignedKey(current.Key)
	if err != nil {
		t.Fatal(err)
	}
	verifier := mustRing(t, current, &previous)
	for _, token := range mintRingTokenKinds(t, legacy) {
		if strings.Contains(token, "kid:") {
			t.Fatal("single-key format changed")
		}
		if err := authenticateRingToken(t, verifier, token); err != nil {
			t.Fatal(err)
		}
	}
	old, err := NewSignedKey(previous.Key)
	if err != nil {
		t.Fatal(err)
	}
	token, err := old.Mint("run-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := authenticateRingToken(t, verifier, token); err == nil {
		t.Fatal("untagged token tried previous key")
	}
}

func TestUnknownKeyIDRejectsAndCannotBeRewritten(t *testing.T) {
	// Even identical key material does not authorize an unknown ID or an ID edit.
	current, other := ringKey("current", 'c'), ringKey("other", 'c')
	signer, verifier := mustRing(t, other, nil), mustRing(t, current, nil)
	for _, token := range mintRingTokenKinds(t, signer) {
		if err := authenticateRingToken(t, verifier, token); err == nil {
			t.Fatal("unknown kid admitted")
		}
		changed := strings.Replace(token, "kid:other.", "kid:current.", 1)
		if err := authenticateRingToken(t, verifier, changed); err == nil {
			t.Fatal("kid was not MAC-bound")
		}
	}
}

func TestKeyFileReloadValidationAndDomainSeparation(t *testing.T) {
	old, newKey := ringKey("old", 'o'), ringKey("new", 'n')
	path := filepath.Join(t.TempDir(), "keys")
	writeRing(t, path, old, &newKey)
	signer, err := LoadDerivedSignedKeyFile(path, CredentialGrantKeyDomain)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := signer.MintCredentialGrant(testCredentialGrant(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	raw := mustRing(t, old, &newKey)
	if _, err := raw.VerifyCredentialGrant(token); err == nil {
		t.Fatal("raw key accepted derived grant")
	}
	for _, invalid := range [][]byte{[]byte(`{"current":{"id":"old","key":"bad"}}`), []byte(`{"unknown":true}`), bytes.Repeat([]byte("x"), maxKeyFileBytes+1)} {
		if err := os.WriteFile(path, invalid, 0600); err != nil {
			t.Fatal(err)
		}
		if err := signer.ReloadKeyFile(); err == nil {
			t.Fatal("invalid file accepted")
		}
		if _, err := signer.VerifyCredentialGrant(token); err != nil {
			t.Fatal("failed reload discarded good keys")
		}
	}
	writeRing(t, path, newKey, &old)
	if err := signer.ReloadKeyFile(); err != nil {
		t.Fatal(err)
	}
	if _, err := signer.VerifyCredentialGrant(token); err != nil {
		t.Fatal(err)
	}
	minted, _, err := signer.MintCredentialGrant(testCredentialGrant(), time.Hour)
	if err != nil || !strings.Contains(minted, "kid:new.") {
		t.Fatalf("promotion: %v", err)
	}
}

func TestKeyRingConcurrentReloadAndWatch(t *testing.T) {
	a, b := ringKey("a", 'a'), ringKey("b", 'b')
	path := filepath.Join(t.TempDir(), "keys")
	writeRing(t, path, a, &b)
	s, err := LoadSignedKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	verifier := mustRing(t, a, &b)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 100 {
				token, err := s.Mint("run", time.Minute)
				if err != nil {
					t.Error(err)
					return
				}
				if err := authenticateRingToken(t, verifier, token); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	for range 10 {
		writeRing(t, path, b, &a)
		if err := s.ReloadKeyFile(); err != nil {
			t.Fatal(err)
		}
		writeRing(t, path, a, &b)
		if err := s.ReloadKeyFile(); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	reported := make(chan error, 10)
	go func() {
		done <- s.WatchKeyFile(ctx, time.Millisecond, func(err error) {
			select {
			case reported <- err:
			default:
			}
		})
	}()
	if err := os.WriteFile(path, []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reported:
	case <-time.After(time.Second):
		t.Fatal("invalid replacement not reported")
	}
	writeRing(t, path, b, &a)
	deadline := time.Now().Add(time.Second)
	for {
		token, err := s.Mint("run", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(token, "kid:b.") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("watch did not reload")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestKeyRingRejectsInvalidConfiguration(t *testing.T) {
	valid := ringKey("valid", 'v')
	for _, bad := range []SigningKey{ringKey("", 'x'), ringKey("a.b", 'x'), ringKey("a:b", 'x'), ringKey("a b", 'x'), {ID: "short", Key: []byte("x")}} {
		if _, err := NewKeyRing(bad, nil); err == nil {
			t.Fatal("invalid current key accepted")
		}
		if _, err := NewKeyRing(valid, &bad); err == nil {
			t.Fatal("invalid previous key accepted")
		}
	}
	if _, err := NewKeyRing(valid, &valid); err == nil {
		t.Fatal("duplicate ID accepted")
	}
	s := mustRing(t, valid, nil)
	if err := s.ReloadKeyFile(); err == nil {
		t.Fatal("fileless reload accepted")
	}
	if err := s.WatchKeyFile(t.Context(), time.Second, func(error) {}); err == nil {
		t.Fatal("fileless watcher accepted")
	}
}

// sign constructs malformed legacy payloads for the existing verifier fixtures.
func (s *SignedKey) sign(payload string) string { return signWith(s.keys.Load().current.Key, payload) }
