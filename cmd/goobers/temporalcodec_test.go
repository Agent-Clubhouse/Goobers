package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

func TestDoctorTemporalCodecAndServerRefusal(t *testing.T) {
	root := initDemo(t)
	var stdout, stderr bytes.Buffer
	if code := runDoctor([]string{"--temporal-codec", "--report", "json", root}, &stdout, &stderr); code != 0 {
		t.Fatalf("doctor=%d: %s", code, stderr.String())
	}
	var status struct {
		Enabled, Strict bool
		Verification    string
	}
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Enabled || status.Strict || !strings.Contains(status.Verification, "configuration-only") {
		t.Fatal("incorrect disabled report")
	}
	configureTestTemporalCodec(t, root)
	stdout.Reset()
	if code := runDoctor([]string{"--temporal-codec", "--report", "json", root}, &stdout, &stderr); code != 0 {
		t.Fatalf("doctor=%d: %s", code, stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.Enabled || status.Strict {
		t.Fatal("incorrect enabled report")
	}
	stderr.Reset()
	if code := runTemporalCodecServer([]string{root}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "api.auth.oidc") {
		t.Fatalf("server accepted local anonymous configuration: %d %s", code, stderr.String())
	}
}

func configureTestTemporalCodec(t *testing.T, root string) *instance.Config {
	t.Helper()
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	keys := t.TempDir()
	if err := os.Chmod(keys, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(keys, "history"), 0700); err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keys, "history", "v1.pem"), pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.SecretStores = append(cfg.SecretStores, instance.SecretStoreConfig{Name: "codec-keys", Kind: instance.SecretStoreKindFileKey, Directory: keys})
	cfg.Temporal = &instance.TemporalConfig{PayloadCodec: &instance.PayloadCodecConfig{KeyRef: &instance.KeyRef{Store: "codec-keys", Name: "history", Version: "v1"}}}
	cfg.Engine = &instance.EngineConfig{HostPort: "127.0.0.1:7233", Namespace: "default", TaskQueue: "codec-test"}
	if err := instance.WriteConfig(instance.NewLayout(root).ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}
	cfg, err = instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
