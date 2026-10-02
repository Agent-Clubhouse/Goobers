package podauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/launchreceipt"
	"github.com/goobers/goobers/internal/readservice"
)

func launchFixture() launchreceipt.Receipt {
	return launchreceipt.Receipt{Version: 1, Binding: launchreceipt.Binding{RunID: "run-1", Stage: "build", StartedSeq: 7, Number: 1, AttemptID: journal.StageAttemptID("run-1", 0, "build", 7)},
		Facts: launchreceipt.RemoteFacts{Source: "control-plane-prepared", ImageReferenceDigest: launchreceipt.Digest([]byte("image")), SelectorDigest: launchreceipt.Digest(nil), Seccomp: "unknown", ContainerCount: 1, NetworkEnforcement: "unknown", SandboxEnforcement: "unknown", ResolvedModel: "unknown", ResolvedEffort: "unknown"}}
}

func launchToken(t *testing.T, key *SignedKey, r launchreceipt.Receipt) string {
	t.Helper()
	raw, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	token, err := key.MintLaunchGrant(launchreceipt.Grant{AttemptID: r.Binding.AttemptID, Digest: launchreceipt.Digest(raw)}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestLaunchGrantForgeryExpiryAndDomainSeparation(t *testing.T) {
	now := time.Now()
	key := grantKey(t, 7, &now)
	r := launchFixture()
	token := launchToken(t, key, r)
	if _, err := key.VerifyLaunchGrant(token); err != nil {
		t.Fatal(err)
	}
	pod, _ := key.Mint("run-1", time.Minute)
	worker, _ := key.MintWorkerConfigDigest("worker", time.Minute)
	credential, _, _ := key.MintCredentialGrant(testCredentialGrant(), time.Minute)
	for _, bad := range []string{token + "x", strings.Repeat("x", 1025), pod, worker, credential,
		launchreceipt.TokenPrefix + strings.TrimPrefix(pod, tokenPrefix), launchreceipt.TokenPrefix + strings.TrimPrefix(worker, workerTokenPrefix)} {
		if _, err := key.VerifyLaunchGrant(bad); err == nil {
			t.Fatal("foreign or forged grant admitted")
		}
	}
	other := grantKey(t, 9, &now)
	if _, err := other.VerifyLaunchGrant(token); err == nil {
		t.Fatal("foreign key admitted")
	}
	if _, _, err := key.verifyToken(tokenPrefix + strings.TrimPrefix(token, launchreceipt.TokenPrefix)); err == nil {
		t.Fatal("launch grant widened to pod authority")
	}
	// Even correctly signed payloads cannot claim an unbounded lifetime.
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"attemptId":"` + r.Binding.AttemptID + `","digest":"` + launchreceipt.Digest(nil) + `","expires":9999999999}`))
	if _, err := key.VerifyLaunchGrant(launchreceipt.TokenPrefix + payload + "." + key.sign(launchMACDomain+payload)); err == nil {
		t.Fatal("unbounded expiry admitted")
	}
	now = now.Add(time.Minute)
	if _, err := key.VerifyLaunchGrant(token); err == nil {
		t.Fatal("expired grant admitted")
	}
}

func TestLaunchReceiptSingleUseCrossAttemptAndRestart(t *testing.T) {
	now := time.Now()
	key := grantKey(t, 7, &now)
	root := t.TempDir()
	store, err := launchreceipt.NewStore(root, key)
	if err != nil {
		t.Fatal(err)
	}
	r := launchFixture()
	token := launchToken(t, key, r)
	other := r
	other.Binding.StartedSeq++
	other.Binding.AttemptID = journal.StageAttemptID("run-1", 0, "build", other.Binding.StartedSeq)
	if err := store.Accept(t.Context(), token, other); !errors.Is(err, launchreceipt.ErrInvalid) {
		t.Fatalf("cross-attempt: %v", err)
	}
	other = r
	other.Facts.HostNetwork = true
	if err := store.Accept(t.Context(), token, other); !errors.Is(err, launchreceipt.ErrInvalid) {
		t.Fatalf("broadened facts: %v", err)
	}
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := store.Accept(context.Background(), token, r)
			if err == nil {
				accepted.Add(1)
			} else if !errors.Is(err, launchreceipt.ErrUsed) {
				t.Errorf("consume: %v", err)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("accepted %d", accepted.Load())
	}
	reopened, err := launchreceipt.NewStore(root, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Accept(t.Context(), token, r); !errors.Is(err, launchreceipt.ErrUsed) {
		t.Fatalf("stolen grant after restart: %v", err)
	}
	// A new mint cannot replace the immutable attempt receipt either.
	if err := reopened.Accept(t.Context(), launchToken(t, key, r), r); !errors.Is(err, launchreceipt.ErrUsed) {
		t.Fatalf("remint: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, r.Binding.AttemptID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := r.Encode()
	if !bytes.Equal(raw, want) {
		t.Fatal("receipt changed")
	}
}

func TestLaunchReceiptCrashResidueAndStorageFailure(t *testing.T) {
	now := time.Now()
	key := grantKey(t, 7, &now)
	r := launchFixture()
	token := launchToken(t, key, r)
	root := t.TempDir()
	store, err := launchreceipt.NewStore(root, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, r.Binding.AttemptID+".json"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.Accept(t.Context(), token, r); !errors.Is(err, launchreceipt.ErrUsed) {
		t.Fatalf("crash residue: %v", err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := store.Accept(t.Context(), token, r); err == nil {
		t.Fatal("storage failure accepted")
	}
}

type launchUnusedReader struct{ readservice.Reader }

func TestLaunchReceiptHTTPAuthorityAndClient(t *testing.T) {
	now := time.Now()
	key := grantKey(t, 7, &now)
	root := t.TempDir()
	store, err := launchreceipt.NewStore(root, key)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := NewAuthenticator(key, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(launchUnusedReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), httpapi.WithAuthenticator(auth), httpapi.WithLaunchReceiptService(store))
	if err != nil {
		t.Fatal(err)
	}
	r := launchFixture()
	raw, _ := r.Encode()
	grant := launchToken(t, key, r)
	pod, _ := key.Mint("run-1", time.Minute)
	worker, _ := key.MintWorkerConfigDigest("worker", time.Minute)
	scoped, _ := key.MintScoped("run-1", time.Minute, ScopeJournal)
	for _, token := range []string{"", pod, worker, scoped, grant + "forged"} {
		req := httptest.NewRequest(http.MethodPost, apicontract.LaunchReceiptPath, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusForbidden && response.Code != http.StatusUnauthorized {
			t.Fatalf("unrelated authority status %d", response.Code)
		}
	}
	// A valid grant cannot use any other route, including human/admin reads.
	for _, path := range []string{apicontract.ConfigDigestPath, "/api/v1/runs/run-1/journal/emit"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+grant)
		principal, err := auth.Authenticate(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = principal // The request principal is installed by the real router.
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code >= 200 && response.Code < 300 {
			t.Fatal("grant widened to another route")
		}
	}
	// Invalid/oversized bodies never consume a valid grant and never echo secrets.
	for _, body := range []string{`{"secret":"do-not-echo"}`, strings.Repeat("x", launchreceipt.MaxBytes+1), string(raw) + `{}`} {
		req := httptest.NewRequest(http.MethodPost, apicontract.LaunchReceiptPath, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+grant)
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusBadRequest || strings.Contains(response.Body.String(), "do-not-echo") {
			t.Fatalf("unsafe malformed-body response: %d %s", response.Code, response.Body)
		}
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	client := launchreceipt.Client{BaseURL: server.URL, Minter: key}
	if err := client.Record(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if err := client.Record(t.Context(), r); err == nil {
		t.Fatal("client replay succeeded")
	}
	files, err := os.ReadDir(root)
	if err != nil || len(files) != 1 {
		t.Fatalf("stored files: %v %v", files, err)
	}
}
