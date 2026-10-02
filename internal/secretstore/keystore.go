package secretstore

import (
	"context"
	"fmt"

	"github.com/goobers/goobers/internal/instance"
)

// KeyStore wraps data keys with RSA-OAEP-256. Implementations never cache
// unwrapped material; callers own the returned plaintext and its lifetime.
// Wrap returns the backend version to persist with ciphertext. Unwrap requires
// that explicit version, so rotation never silently selects a different key.
type KeyStore interface {
	Wrap(ctx context.Context, ref instance.KeyRef, plaintext []byte) (ciphertext []byte, keyVersion string, err error)
	Unwrap(ctx context.Context, ref instance.KeyRef, ciphertext []byte) (plaintext []byte, err error)
}

// KeyRegistry resolves typed key refs across explicitly declared key stores.
// Construction opens no files and performs no key operations.
type KeyRegistry struct{ stores map[string]KeyStore }

// NewKeyRegistry builds only the key entries in secretStores. Secret stores
// remain available exclusively through NewRegistry and FetchSecret.
func NewKeyRegistry(configs []instance.SecretStoreConfig) (*KeyRegistry, error) {
	stores := make(map[string]KeyStore)
	seen := make(map[string]bool)
	for _, cfg := range configs {
		if seen[cfg.Name] {
			return nil, fmt.Errorf("secretstore: store %q is declared more than once", cfg.Name)
		}
		seen[cfg.Name] = true
		if err := cfg.Validate(); err != nil {
			return nil, err
		}
		var store KeyStore
		switch cfg.Kind {
		case instance.SecretStoreKindAzureKeyVault:
			continue
		case instance.SecretStoreKindFileKey:
			store = &fileKeyStore{directory: cfg.Directory}
		case instance.SecretStoreKindKeyVaultKey:
			credential, err := azureStoreCredential(cfg)
			if err != nil {
				return nil, err
			}
			store, err = newAzureKeyStore(cfg.VaultURI, credential, nil)
			if err != nil {
				return nil, err
			}
		}
		stores[cfg.Name] = store
	}
	return &KeyRegistry{stores: stores}, nil
}

func (r *KeyRegistry) resolve(ref instance.KeyRef) (KeyStore, error) {
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	if r != nil {
		if store := r.stores[ref.Store]; store != nil {
			return store, nil
		}
	}
	return nil, fmt.Errorf("secretstore: key store %q is not declared", ref.Store)
}

// Wrap wraps plaintext and returns the selected backend key version.
func (r *KeyRegistry) Wrap(ctx context.Context, ref instance.KeyRef, plaintext []byte) ([]byte, string, error) {
	store, err := r.resolve(ref)
	if err != nil {
		return nil, "", err
	}
	return store.Wrap(ctx, ref, plaintext)
}

// Unwrap requires the exact backend version returned when wrapping.
func (r *KeyRegistry) Unwrap(ctx context.Context, ref instance.KeyRef, ciphertext []byte) ([]byte, error) {
	store, err := r.resolve(ref)
	if err != nil {
		return nil, err
	}
	return store.Unwrap(ctx, ref, ciphertext)
}

func validateKeyOperation(ctx context.Context, ref instance.KeyRef, value []byte, unwrap bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	if unwrap && ref.Version == "" {
		return fmt.Errorf("unwrap requires an explicit key version")
	}
	if len(value) == 0 || len(value) > 1024 {
		return fmt.Errorf("key operation input must contain 1 to 1024 bytes")
	}
	return nil
}
