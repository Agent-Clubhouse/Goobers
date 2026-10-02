package instance

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestKeyStoreValidation(t *testing.T) {
	file := SecretStoreConfig{Name: "local", Kind: SecretStoreKindFileKey, Directory: filepath.Join(t.TempDir(), "keys")}
	vault := validSecretStore()
	vault.Name = "keys"
	vault.Kind = SecretStoreKindKeyVaultKey
	for _, cfg := range []SecretStoreConfig{file, vault} {
		t.Run(cfg.Kind, func(t *testing.T) {
			c := Config{SecretStores: []SecretStoreConfig{cfg}}
			if err := c.Validate(); err != nil {
				t.Fatal(err)
			}
			if err := c.ValidateKeyRef(KeyRef{Store: cfg.Name, Name: "data", Version: "v1"}); err != nil {
				t.Fatal(err)
			}
			c.Webhook.Secret = TokenRef{Store: cfg.Name + "/data"}
			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "cannot fetch secrets") {
				t.Fatalf("secret use: %v", err)
			}
			c.Webhook.Secret = TokenRef{}
			c.SecretStores = append(c.SecretStores, cfg)
			if err := c.Validate(); err == nil {
				t.Fatal("accepted duplicate key store")
			}
		})
	}
	secret := Config{SecretStores: []SecretStoreConfig{validSecretStore()}}
	if err := secret.ValidateKeyRef(KeyRef{Store: secret.SecretStores[0].Name, Name: "data"}); err == nil {
		t.Fatal("accepted secret store as key store")
	}
	for _, mutate := range []func(*SecretStoreConfig){
		func(c *SecretStoreConfig) { c.Directory = "relative" },
		func(c *SecretStoreConfig) { c.Auth = &SecretStoreAuthConfig{Kind: SecretStoreAuthAzureCLI} },
		func(c *SecretStoreConfig) { c.VaultURI = "https://example.com" },
		func(c *SecretStoreConfig) { c.CacheTTLSeconds = 1 },
	} {
		cfg := file
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Fatal("accepted invalid file-key config")
		}
	}
	vault.Directory = file.Directory
	if err := vault.Validate(); err == nil {
		t.Fatal("accepted directory on Azure store")
	}
}

func TestKeyRefRejectsInvalidSegments(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "../escape", "a/b", "a\\b", "a%2fb", "a?b", "a#b", "a b", strings.Repeat("a", 128)} {
		if err := (KeyRef{Store: "keys", Name: bad}).Validate(); err == nil {
			t.Errorf("accepted name %q", bad)
		}
		if bad != "" {
			if err := (KeyRef{Store: "keys", Name: "data", Version: bad}).Validate(); err == nil {
				t.Errorf("accepted version %q", bad)
			}
		}
	}
}
