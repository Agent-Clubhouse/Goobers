package instance

import (
	"fmt"
	"path/filepath"
)

// KeyRef identifies a wrapping key, separately from a secret-valued TokenRef.
// Version is empty only when wrapping with the backend's current key. Persist
// the version returned by Wrap with its ciphertext and supply it to Unwrap.
// Versions are backend identifiers, not application signing-key IDs.
type KeyRef struct {
	Store   string `json:"store" yaml:"store"`
	Name    string `json:"name" yaml:"name"`
	Version string `json:"version,omitempty" yaml:"version,omitempty"`
}

// Validate checks the reference shape. Unwrap additionally requires a version.
func (r KeyRef) Validate() error {
	if !validSecretStoreName(r.Store) || !ValidKeyComponent(r.Name) {
		return fmt.Errorf("key ref requires a store DNS label and an alphanumeric/hyphen key name")
	}
	if r.Version != "" && !ValidKeyComponent(r.Version) {
		return fmt.Errorf("key ref version must contain only letters, digits, and hyphens (at most 127 characters)")
	}
	return nil
}

// ValidKeyComponent restricts key names and versions to safe URL/path segments.
func ValidKeyComponent(s string) bool {
	if len(s) == 0 || len(s) > 127 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

// ValidateKeyRef checks that a typed key reference addresses a key store.
func (c *Config) ValidateKeyRef(ref KeyRef) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	stores, err := c.validateSecretStores()
	if err != nil {
		return err
	}
	secret, ok := stores[ref.Store]
	if !ok {
		return fmt.Errorf("key store %q is not declared under secretStores", ref.Store)
	}
	if secret {
		return fmt.Errorf("key ref names secret store %q, which cannot wrap keys", ref.Store)
	}
	return nil
}

// IsKeyStore reports whether this declaration provides wrapping operations.
func (c SecretStoreConfig) IsKeyStore() bool {
	return c.Kind == SecretStoreKindKeyVaultKey || c.Kind == SecretStoreKindFileKey
}

// Validate checks a standalone store declaration without opening it.
func (c SecretStoreConfig) Validate() error { return c.validate(0, make(map[string]bool)) }

func (c SecretStoreConfig) validateFileKey() error {
	if !filepath.IsAbs(c.Directory) {
		return fmt.Errorf("file-key store %q: directory must be absolute", c.Name)
	}
	if c.Auth != nil || c.VaultURI != "" {
		return fmt.Errorf("file-key store %q: auth and vaultURI are not supported", c.Name)
	}
	return nil
}
