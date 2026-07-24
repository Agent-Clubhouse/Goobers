//go:build integration

package secretstore

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/testdep"
)

// TestIntegrationAzureKeyVaultLiveSmoke resolves one real secret from a real
// vault through the full Registry path, authenticated as the developer's
// current Azure CLI login. Double-gated like the other live smokes: the
// integration build tag AND an explicit env opt-in naming the vault
// (GOOBERS_LIVE_KEYVAULT_URI) plus the secret to read
// (GOOBERS_LIVE_KEYVAULT_SECRET). The value is asserted non-empty and never
// printed.
func TestIntegrationAzureKeyVaultLiveSmoke(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_LIVE_KEYVAULT_URI")
	vaultURI := os.Getenv("GOOBERS_LIVE_KEYVAULT_URI")
	secretName := os.Getenv("GOOBERS_LIVE_KEYVAULT_SECRET")
	if vaultURI == "" || secretName == "" {
		t.Skip("integration test skipped: set GOOBERS_LIVE_KEYVAULT_URI and GOOBERS_LIVE_KEYVAULT_SECRET to opt in")
	}

	registry, err := NewRegistry([]instance.SecretStoreConfig{{
		Name:     "live-kv",
		Kind:     instance.SecretStoreKindAzureKeyVault,
		VaultURI: vaultURI,
		Auth:     &instance.SecretStoreAuthConfig{Kind: instance.SecretStoreAuthAzureCLI},
	}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	value, err := registry.FetchSecret(ctx, "live-kv/"+secretName)
	if err != nil {
		t.Fatalf("FetchSecret: %v", err)
	}
	if value == "" {
		t.Fatal("FetchSecret returned an empty value")
	}
	// Cached repeat resolve must agree without a second vault round-trip
	// (indirectly: it must at least agree).
	again, err := registry.FetchSecret(ctx, "live-kv/"+secretName)
	if err != nil {
		t.Fatalf("FetchSecret (cached): %v", err)
	}
	if again != value {
		t.Fatal("cached resolve disagreed with the first resolve")
	}
}
