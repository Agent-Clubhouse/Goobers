package secretstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azkeys"

	"github.com/goobers/goobers/internal/instance"
)

type fakeKeyService struct {
	key   *rsa.PrivateKey
	kid   string
	calls int
	deny  bool
}

func (f *fakeKeyService) Do(req *http.Request) (*http.Response, error) {
	header := http.Header{"Content-Type": []string{"application/json"}}
	response := func(status int, body string) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	}
	if req.Header.Get("Authorization") == "" {
		header.Set("WWW-Authenticate", `Bearer authorization="https://login.microsoftonline.com/00000000-0000-0000-0000-000000000000", resource="https://vault.azure.net"`)
		return response(http.StatusUnauthorized, "")
	}
	f.calls++
	if f.deny || req.Header.Get("Authorization") != "Bearer fake-bearer-token" {
		return response(http.StatusForbidden, `{"error":{"code":"Forbidden","message":"sensitive backend detail"}}`)
	}
	var body struct {
		Alg   string `json:"alg"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		return nil, err
	}
	if body.Alg != "RSA-OAEP-256" {
		return nil, fmt.Errorf("unexpected algorithm")
	}
	value, err := base64.RawURLEncoding.DecodeString(body.Value)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(req.URL.Path, "/keys/data/") {
		return response(http.StatusNotFound, `{"error":{"code":"KeyNotFound"}}`)
	}
	switch {
	case strings.HasSuffix(req.URL.Path, "/wrapkey"):
		value, err = rsa.EncryptOAEP(sha256.New(), rand.Reader, &f.key.PublicKey, value, nil)
	case strings.HasSuffix(req.URL.Path, "/unwrapkey"):
		value, err = rsa.DecryptOAEP(sha256.New(), rand.Reader, f.key, value, nil)
	default:
		return nil, fmt.Errorf("unexpected operation")
	}
	if err != nil {
		return response(http.StatusBadRequest, `{"error":{"code":"BadParameter"}}`)
	}
	return response(http.StatusOK, fmt.Sprintf(`{"kid":%q,"value":%q}`, f.kid, base64.RawURLEncoding.EncodeToString(value)))
}
func fakeAzureKeys(t *testing.T) (KeyStore, *fakeKeyService) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	service := &fakeKeyService{key: key, kid: "https://unit-kv.vault.azure.net/keys/data/v1"}
	store, err := newAzureKeyStore("https://unit-kv.vault.azure.net", fakeTokenCredential{}, &azkeys.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: service}})
	if err != nil {
		t.Fatal(err)
	}
	return store, service
}

func TestAzureKeysRoundTripAndNoUnwrapCache(t *testing.T) {
	store, service := fakeAzureKeys(t)
	ctx := context.Background()
	ref := instance.KeyRef{Store: "keys", Name: "data"}
	plain := bytes.Repeat([]byte{17}, 32)
	ciphertext, version, err := store.Wrap(ctx, ref, plain)
	if err != nil {
		t.Fatal(err)
	}
	if version != "v1" {
		t.Fatal("missing concrete version")
	}
	ref.Version = version
	for range 2 {
		got, err := store.Unwrap(ctx, ref, ciphertext)
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("roundtrip: %v", err)
		}
	}
	if service.calls != 3 {
		t.Fatalf("calls = %d, want 3", service.calls)
	}
	ciphertext[0] ^= 1
	if got, err := store.Unwrap(ctx, ref, ciphertext); err == nil || got != nil {
		t.Fatal("accepted tampering")
	}
	ref.Name = "wrong"
	if _, err := store.Unwrap(ctx, ref, ciphertext); err == nil {
		t.Fatal("accepted wrong key")
	}
}

func TestAzureKeysRejectMismatchedResponses(t *testing.T) {
	store, service := fakeAzureKeys(t)
	ctx := context.Background()
	ref := instance.KeyRef{Store: "keys", Name: "data", Version: "v1"}
	ciphertext, _, err := store.Wrap(ctx, ref, []byte("key"))
	if err != nil {
		t.Fatal(err)
	}
	for _, kid := range []string{"", "https://other.vault.azure.net/keys/data/v1", "https://unit-kv.vault.azure.net/keys/wrong/v1", "https://unit-kv.vault.azure.net/keys/data/v2", "https://unit-kv.vault.azure.net/keys/data/v1?x=y", "https://unit-kv.vault.azure.net/keys/data/v1/extra"} {
		service.kid = kid
		if got, version, err := store.Wrap(ctx, ref, []byte("key")); err == nil || got != nil || version != "" {
			t.Fatalf("wrap accepted %q", kid)
		}
		if got, err := store.Unwrap(ctx, ref, ciphertext); err == nil || got != nil {
			t.Fatalf("unwrap accepted %q", kid)
		}
	}
}

func TestAzureKeysFailClosed(t *testing.T) {
	store, service := fakeAzureKeys(t)
	ctx := context.Background()
	ref := instance.KeyRef{Store: "keys", Name: "data"}
	if _, err := store.Unwrap(ctx, ref, []byte("ciphertext")); err == nil {
		t.Fatal("accepted missing version")
	}
	if service.calls != 0 {
		t.Fatal("contacted service without version")
	}
	service.deny = true
	if _, _, err := store.Wrap(ctx, ref, []byte("key")); err == nil || strings.Contains(err.Error(), "sensitive backend detail") {
		t.Fatalf("auth failure: %v", err)
	}
	clearAzureIdentityEnv(t)
	for _, auth := range []string{instance.SecretStoreAuthWorkloadIdentity, "default-azure-credential"} {
		cfg := azureStoreConfig(auth, "")
		cfg.Kind = instance.SecretStoreKindKeyVaultKey
		if _, err := NewKeyRegistry([]instance.SecretStoreConfig{cfg}); err == nil {
			t.Fatalf("accepted unavailable auth %s", auth)
		}
	}
	for _, auth := range []string{instance.SecretStoreAuthManagedIdentity, instance.SecretStoreAuthAzureCLI} {
		cfg := azureStoreConfig(auth, "")
		cfg.Kind = instance.SecretStoreKindKeyVaultKey
		if _, err := NewKeyRegistry([]instance.SecretStoreConfig{cfg}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRegistriesSeparateKeysAndSecrets(t *testing.T) {
	configs := []instance.SecretStoreConfig{azureStoreConfig(instance.SecretStoreAuthWorkloadIdentity, ""), {Name: "local", Kind: instance.SecretStoreKindFileKey, Directory: "/nonexistent"}}
	configs[0].Kind = instance.SecretStoreKindKeyVaultKey
	clearAzureIdentityEnv(t)
	secrets, err := NewRegistry(configs)
	if err != nil {
		t.Fatalf("secret registry activated key store: %v", err)
	}
	if _, err := secrets.FetchSecret(context.Background(), "local/data"); err == nil {
		t.Fatal("resolved key as secret")
	}
	keys, err := NewKeyRegistry([]instance.SecretStoreConfig{azureStoreConfig(instance.SecretStoreAuthWorkloadIdentity, "")})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := keys.Wrap(context.Background(), instance.KeyRef{Store: "unit-kv", Name: "data"}, []byte("key")); err == nil {
		t.Fatal("resolved secret as key")
	}
	configs = append(configs, configs[0])
	if _, err := NewRegistry(configs); err == nil {
		t.Fatal("accepted duplicate skipped entries")
	}
}
