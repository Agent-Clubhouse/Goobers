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
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	shared "github.com/goobers/goobers/internal/podauth"
)

type testRevocations struct {
	revoked bool
	err     error
}

func (r *testRevocations) Revoke(context.Context, string, time.Time) error {
	r.revoked = true
	return r.err
}
func (r *testRevocations) IsRevoked(context.Context, string) (bool, error) { return r.revoked, r.err }

func (r *testRevocations) CheckGrant(context.Context, string, time.Time) error {
	if r.err != nil {
		return r.err
	}
	if r.revoked {
		return shared.ErrInvalidCredentialGrant
	}
	return nil
}

func authFixture(t *testing.T, store *testRevocations) *Auth {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key")
	raw, err := json.Marshal(map[string]shared.SigningKey{"current": {ID: "v1", Key: bytes.Repeat([]byte("k"), 32)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{Mode: "signed", KeyFile: path, Revocations: store, Fallback: httpapi.DenyAllAuthenticator{}})
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func TestCloudRefusesRegistryMode(t *testing.T) {
	for _, config := range []Config{{}, {Mode: "registry"}, {Mode: "signed"}, {Mode: "signed", KeyFile: "unused"}} {
		if _, err := New(config); err == nil {
			t.Fatalf("accepted incomplete cloud auth: %+v", config)
		}
	}
}
func TestCloudGrantAdmissionFailsClosed(t *testing.T) {
	store := &testRevocations{}
	a := authFixture(t, store)
	token, _, err := a.GrantKeys.MintCredentialGrant(shared.CredentialGrant{RunID: "run", Stage: "stage", Capabilities: []string{"repo:push"}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/credentials/refresh", nil)
	req.Header.Set("Authorization", "bEaReR "+token)
	principal, err := a.Authenticate(req)
	if err != nil || principal.Subject != "run:run" || principal.Issuer != httpapi.CredentialGrantPrincipalIssuer {
		t.Fatalf("valid grant: %+v %v", principal, err)
	}
	store.revoked = true
	if _, err := a.Authenticate(req); !errors.Is(err, shared.ErrInvalidCredentialGrant) {
		t.Fatalf("revoked grant: %v", err)
	}
	store.revoked = false
	store.err = errors.New("database unavailable")
	if _, err := a.Authenticate(req); !errors.Is(err, store.err) {
		t.Fatalf("read failure admitted grant: %v", err)
	}
	if _, err := a.VerifyCredentialGrant(t.Context(), "invalid"); err == nil {
		t.Fatal("invalid grant admitted")
	}
	pod, err := a.PodKeys.MintScoped("run", time.Hour, shared.ScopeClaims)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+pod)
	principal, err = a.Authenticate(req)
	if err != nil || principal.Issuer != httpapi.PodPrincipalIssuer {
		t.Fatalf("pod routing changed: %+v %v", principal, err)
	}
}
func TestCloudKeyReloadStops(t *testing.T) {
	a := authFixture(t, &testRevocations{})
	if err := a.RunKeyReload(t.Context(), nil); err == nil {
		t.Fatal("missing error reporter accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := a.RunKeyReload(ctx, func(err error) { t.Error(err) }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestCloudRequiresExplicitJSONRing(t *testing.T) {
	for name, raw := range map[string][]byte{
		"raw":       bytes.Repeat([]byte("k"), 64),
		"brace-raw": append([]byte("{"), bytes.Repeat([]byte("k"), 63)...),
		"malformed": []byte(`{"current":{"id":"v1","key":"invalid"}}`),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "keys.json")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := New(Config{Mode: "signed", KeyFile: path, Revocations: &testRevocations{}}); err == nil {
				t.Fatal("cloud boot accepted a non-ring file")
			}
		})
	}
}

// A child-workflow grant control minted authenticates as the child-workflow
// principal for its run; one signed by another key does not.
func TestCloudAuthenticatesChildWorkflowGrants(t *testing.T) {
	a := authFixture(t, &testRevocations{})
	grant := shared.ChildWorkflowGrant{Gaggle: "web", RunID: "0123456789abcdef0123456789abcdef", StageOccurrence: "occ", AttemptID: "att",
		ConfigDigest: "sha256:" + string(bytes.Repeat([]byte("a"), 64)), PolicyDigest: "sha256:" + string(bytes.Repeat([]byte("b"), 64))}
	token, _, err := a.GrantKeys.MintChildWorkflowGrant(grant, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	principal, err := a.Authenticate(request)
	if err != nil || principal.Issuer != httpapi.ChildWorkflowPrincipalIssuer || principal.Subject != "run:"+grant.RunID {
		t.Fatalf("principal = %+v, %v", principal, err)
	}
	other, err := shared.NewSignedKey(bytes.Repeat([]byte("z"), 32))
	if err != nil {
		t.Fatal(err)
	}
	forged, _, err := other.MintChildWorkflowGrant(grant, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+forged)
	if principal, err := a.Authenticate(request); err == nil {
		t.Fatalf("a grant from another key authenticated: %+v", principal)
	}
}
