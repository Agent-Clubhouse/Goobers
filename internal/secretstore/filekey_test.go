package secretstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

func provisionFileKey(t *testing.T, root, name, version string) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
		t.Fatal(err)
	}
	writeKeyFile(t, filepath.Join(root, name, version+".pem"), pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	return key
}
func writeKeyFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func localKeyRegistry(t *testing.T) (*KeyRegistry, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	registry, err := NewKeyRegistry([]instance.SecretStoreConfig{{Name: "local", Kind: instance.SecretStoreKindFileKey, Directory: root}})
	if err != nil {
		t.Fatal(err)
	}
	return registry, root
}

func TestFileKeyRotationAndFailures(t *testing.T) {
	registry, root := localKeyRegistry(t)
	provisionFileKey(t, root, "data", "v1")
	provisionFileKey(t, root, "data", "v2")
	marker := filepath.Join(root, "data", "active")
	writeKeyFile(t, marker, []byte("v1\n"))
	ref := instance.KeyRef{Store: "local", Name: "data"}
	plain := bytes.Repeat([]byte{42}, 32)
	ctx := context.Background()
	ciphertext, version, err := registry.Wrap(ctx, ref, plain)
	if err != nil {
		t.Fatal(err)
	}
	if version != "v1" {
		t.Fatal("wrong version")
	}
	writeKeyFile(t, marker, []byte("v2\n"))
	_, latest, err := registry.Wrap(ctx, ref, plain)
	if err != nil || latest != "v2" {
		t.Fatalf("rotation: %s %v", latest, err)
	}
	ref.Version = version
	got, err := registry.Unwrap(ctx, ref, ciphertext)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("roundtrip: %v", err)
	}
	for _, version := range []string{"", "v2", "missing"} {
		ref.Version = version
		if got, err := registry.Unwrap(ctx, ref, ciphertext); err == nil || got != nil {
			t.Fatalf("accepted version %q", version)
		}
	}
	ref.Version = "v1"
	ciphertext[0] ^= 1
	if got, err := registry.Unwrap(ctx, ref, ciphertext); err == nil || got != nil {
		t.Fatal("accepted tampered ciphertext")
	}
	if _, _, err := registry.Wrap(ctx, ref, make([]byte, 191)); err == nil {
		t.Fatal("accepted oversized plaintext")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := registry.Wrap(canceled, ref, plain); err == nil {
		t.Fatal("ignored cancellation")
	}
	// Removal after a successful read proves keys and unwrapped values aren't cached.
	if err := os.Remove(filepath.Join(root, "data", "v1.pem")); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Unwrap(ctx, ref, ciphertext); err == nil {
		t.Fatal("used cached key")
	}
}

func TestFileKeyRejectsUnsafeFiles(t *testing.T) {
	for _, scenario := range []string{"permissions", "symlink", "directory", "large", "bad-pem", "active-traversal", "directory-permissions"} {
		t.Run(scenario, func(t *testing.T) {
			registry, root := localKeyRegistry(t)
			provisionFileKey(t, root, "data", "v1")
			path := filepath.Join(root, "data", "v1.pem")
			writeKeyFile(t, filepath.Join(root, "data", "active"), []byte("v1"))
			switch scenario {
			case "permissions":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := path + ".real"
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "large":
				writeKeyFile(t, path, []byte(strings.Repeat("x", 16385)))
			case "bad-pem":
				writeKeyFile(t, path, []byte("not a private key"))
			case "active-traversal":
				writeKeyFile(t, filepath.Join(root, "data", "active"), []byte("../outside"))
			case "directory-permissions":
				if err := os.Chmod(filepath.Join(root, "data"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := registry.Wrap(context.Background(), instance.KeyRef{Store: "local", Name: "data"}, []byte("data")); err == nil {
				t.Fatal("accepted unsafe key")
			}
		})
	}
}

func TestFileKeyPKCS8AndWeakKeys(t *testing.T) {
	registry, root := localKeyRegistry(t)
	key := provisionFileKey(t, root, "data", "v1")
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "data", "v1.pem")
	writeKeyFile(t, path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}))
	ref := instance.KeyRef{Store: "local", Name: "data", Version: "v1"}
	if _, _, err := registry.Wrap(context.Background(), ref, []byte("key")); err != nil {
		t.Fatal(err)
	}
	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	writeKeyFile(t, path, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(weak)}))
	if _, _, err := registry.Wrap(context.Background(), ref, []byte("key")); err == nil {
		t.Fatal("accepted weak RSA key")
	}
}
