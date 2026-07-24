package secretstore

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"

	"github.com/goobers/goobers/internal/instance"
)

// clearAzureIdentityEnv blanks the ambient AZURE_* variables azidentity reads,
// so construction-error tests are deterministic on a developer host that
// happens to have a real identity configured.
func clearAzureIdentityEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"AZURE_TENANT_ID", "AZURE_CLIENT_ID", "AZURE_FEDERATED_TOKEN_FILE",
		"AZURE_AUTHORITY_HOST", "AZURE_ADDITIONALLY_ALLOWED_TENANTS",
	} {
		t.Setenv(name, "")
	}
}

func azureStoreConfig(authKind, clientID string) instance.SecretStoreConfig {
	return instance.SecretStoreConfig{
		Name:     "unit-kv",
		Kind:     instance.SecretStoreKindAzureKeyVault,
		VaultURI: "https://unit-kv.vault.azure.net",
		Auth:     &instance.SecretStoreAuthConfig{Kind: authKind, ClientID: clientID},
	}
}

func TestAzureStoreCredentialWorkloadIdentityFailsClosedWithoutFederation(t *testing.T) {
	clearAzureIdentityEnv(t)
	if _, err := azureStoreCredential(azureStoreConfig(instance.SecretStoreAuthWorkloadIdentity, "")); err == nil {
		t.Fatal("azureStoreCredential: want workload-identity construction error without AZURE_* federation env, got nil")
	}
}

func TestAzureStoreCredentialConstructsPerAuthKind(t *testing.T) {
	clearAzureIdentityEnv(t)
	for _, tc := range []struct {
		name     string
		authKind string
		clientID string
	}{
		{"managed identity system-assigned", instance.SecretStoreAuthManagedIdentity, ""},
		{"managed identity user-assigned", instance.SecretStoreAuthManagedIdentity, "11111111-1111-1111-1111-111111111111"},
		{"azure cli", instance.SecretStoreAuthAzureCLI, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			credential, err := azureStoreCredential(azureStoreConfig(tc.authKind, tc.clientID))
			if err != nil {
				t.Fatalf("azureStoreCredential: %v", err)
			}
			if credential == nil {
				t.Fatal("azureStoreCredential returned a nil credential")
			}
		})
	}
}

func TestAzureStoreCredentialFailsClosedOnBadConfig(t *testing.T) {
	clearAzureIdentityEnv(t)
	noAuth := azureStoreConfig("", "")
	noAuth.Auth = nil
	for _, tc := range []struct {
		name string
		cfg  instance.SecretStoreConfig
	}{
		{"nil auth", noAuth},
		{"unsupported auth kind", azureStoreConfig("default-azure-credential", "")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := azureStoreCredential(tc.cfg); err == nil {
				t.Fatal("azureStoreCredential: want error, got nil")
			}
		})
	}
}

func TestNewAzureKeyVaultStoreRejectsUnknownKind(t *testing.T) {
	cfg := azureStoreConfig(instance.SecretStoreAuthAzureCLI, "")
	cfg.Kind = "aws-secrets-manager"
	if _, err := newAzureKeyVaultStore(cfg); err == nil {
		t.Fatal("newAzureKeyVaultStore: want unsupported-kind error, got nil")
	}
}

// fakeTokenCredential satisfies azcore.TokenCredential without any identity
// provider round-trip.
type fakeTokenCredential struct{}

func (fakeTokenCredential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "fake-bearer-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

// fakeKeyVaultTransport speaks just enough of the Key Vault REST shape for
// azsecrets.Client: it answers unauthenticated requests with the bearer
// challenge and authenticated GET /secrets/{name} with the stored value.
type fakeKeyVaultTransport struct {
	mu       sync.Mutex
	secrets  map[string]string
	requests int
}

func (f *fakeKeyVaultTransport) Do(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	respond := func(status int, header http.Header, body string) *http.Response {
		if header == nil {
			header = http.Header{}
		}
		return &http.Response{
			StatusCode: status,
			Header:     header,
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}
	}
	if req.Header.Get("Authorization") == "" {
		header := http.Header{}
		header.Set("WWW-Authenticate", `Bearer authorization="https://login.microsoftonline.com/00000000-0000-0000-0000-000000000000", resource="https://vault.azure.net"`)
		return respond(http.StatusUnauthorized, header, ""), nil
	}
	if req.Header.Get("Authorization") != "Bearer fake-bearer-token" {
		return respond(http.StatusForbidden, nil, `{"error":{"message":"bad token"}}`), nil
	}
	name := strings.TrimPrefix(req.URL.Path, "/secrets/")
	name = strings.TrimSuffix(name, "/")
	value, ok := f.secrets[name]
	if !ok {
		return respond(http.StatusNotFound, http.Header{"Content-Type": []string{"application/json"}},
			`{"error":{"code":"SecretNotFound","message":"secret not found"}}`), nil
	}
	header := http.Header{"Content-Type": []string{"application/json"}}
	body := fmt.Sprintf(`{"value":%q,"id":"https://unit-kv.vault.azure.net/secrets/%s/0123456789abcdef"}`, value, name)
	return respond(http.StatusOK, header, body), nil
}

func fakeVaultStore(t *testing.T, secrets map[string]string) (Store, *fakeKeyVaultTransport) {
	t.Helper()
	transport := &fakeKeyVaultTransport{secrets: secrets}
	store, err := newAzureKeyVaultStoreWithCredential(
		"https://unit-kv.vault.azure.net",
		fakeTokenCredential{},
		&azsecrets.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: transport}},
	)
	if err != nil {
		t.Fatalf("newAzureKeyVaultStoreWithCredential: %v", err)
	}
	return store, transport
}

func TestAzureKeyVaultStoreFetchesSecret(t *testing.T) {
	store, _ := fakeVaultStore(t, map[string]string{"github-token": "kv-s3cr3t"})
	got, err := store.FetchSecret(context.Background(), "github-token")
	if err != nil {
		t.Fatalf("FetchSecret: %v", err)
	}
	if got != "kv-s3cr3t" {
		t.Fatalf("FetchSecret = %q, want %q", got, "kv-s3cr3t")
	}
}

func TestAzureKeyVaultStoreFailsClosedOnMissingSecret(t *testing.T) {
	store, _ := fakeVaultStore(t, map[string]string{})
	if _, err := store.FetchSecret(context.Background(), "missing"); err == nil {
		t.Fatal("FetchSecret: want error for missing secret, got nil")
	}
}

func TestRegistryOverFakeVaultTransport(t *testing.T) {
	store, transport := fakeVaultStore(t, map[string]string{"github-token": "kv-s3cr3t"})
	registry, err := newRegistry(
		[]instance.SecretStoreConfig{azureStoreConfig(instance.SecretStoreAuthAzureCLI, "")},
		func(instance.SecretStoreConfig) (Store, error) { return store, nil },
	)
	if err != nil {
		t.Fatalf("newRegistry: %v", err)
	}
	for i := 0; i < 3; i++ {
		got, err := registry.FetchSecret(context.Background(), "unit-kv/github-token")
		if err != nil {
			t.Fatalf("FetchSecret #%d: %v", i, err)
		}
		if got != "kv-s3cr3t" {
			t.Fatalf("FetchSecret = %q, want %q", got, "kv-s3cr3t")
		}
	}
	transport.mu.Lock()
	requests := transport.requests
	transport.mu.Unlock()
	// One challenge round-trip plus one authenticated GET; the TTL cache
	// absorbs the two repeat resolves.
	if requests > 2 {
		t.Fatalf("vault saw %d requests for 3 resolves, want at most 2 (challenge + GET)", requests)
	}
}
