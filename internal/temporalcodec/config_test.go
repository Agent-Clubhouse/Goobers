package temporalcodec

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

func fileCodecConfig(t *testing.T) *instance.Config {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "history"), 0700); err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	data := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(filepath.Join(root, "history", "v1.pem"), data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &instance.Config{SecretStores: []instance.SecretStoreConfig{
		{Name: "local", Kind: instance.SecretStoreKindFileKey, Directory: root},
		{Name: "unused", Kind: instance.SecretStoreKindKeyVaultKey, VaultURI: "https://unused.vault.azure.net", Auth: &instance.SecretStoreAuthConfig{Kind: instance.SecretStoreAuthWorkloadIdentity}},
	}, Temporal: &instance.TemporalConfig{PayloadCodec: &instance.PayloadCodecConfig{KeyRef: &instance.KeyRef{Store: "local", Name: "history", Version: "v1"}}}}
	return cfg
}

func TestConfiguredDataConverterUsesSelectedFileKey(t *testing.T) {
	cfg := fileCodecConfig(t)
	// Construction must not bootstrap the unused workload identity.
	for _, name := range []string{"AZURE_TENANT_ID", "AZURE_CLIENT_ID", "AZURE_FEDERATED_TOKEN_FILE"} {
		t.Setenv(name, "")
	}
	dc, err := DataConverter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := dc.ToPayload("private workflow input")
	if err != nil {
		t.Fatal(err)
	}
	if string(payload.Metadata["encoding"]) != Encoding {
		t.Fatal("explicit constructor did not seal")
	}
	var decoded string
	if err := dc.FromPayload(payload, &decoded); err != nil || decoded != "private workflow input" {
		t.Fatalf("roundtrip: %v", err)
	}
	cfg.Temporal.PayloadCodec.KeyRef.Store = "missing"
	if _, err := DataConverter(cfg); err == nil {
		t.Fatal("accepted missing store")
	}
	cfg.Temporal.PayloadCodec.KeyRef = nil
	cfg.Temporal.PayloadCodec.Strict = true
	if _, err := DataConverter(cfg); err == nil {
		t.Fatal("accepted strict without key")
	}
}
