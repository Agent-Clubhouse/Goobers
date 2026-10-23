package podauth

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRawKeyFileCompatibility(t *testing.T) {
	for name, raw := range map[string][]byte{
		"brace-prefixed-64-bytes": append([]byte("{"), bytes.Repeat([]byte("k"), 63)...),
		"valid-json":              []byte(`{"current":{"id":"v1","key":"not-a-ring-to-this-loader"}}`),
		"over-ring-size-limit":    bytes.Repeat([]byte("k"), maxKeyFileBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "key")
			if err := os.WriteFile(path, append(append([]byte(" \n"), raw...), '\n'), 0600); err != nil {
				t.Fatal(err)
			}
			signer, err := LoadSignedKeyFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if keys := signer.keys.Load(); keys.current.ID != "" || !bytes.Equal(keys.current.Key, raw) {
				t.Fatal("raw loader interpreted or changed trimmed key bytes")
			}
			want, err := deriveSigningKey(raw, CredentialGrantKeyDomain)
			if err != nil {
				t.Fatal(err)
			}
			for _, load := range []func(string) (*SignedKey, error){
				CredentialGrantSigningKey,
				func(path string) (*SignedKey, error) {
					return LoadDerivedSignedKeyFile(path, CredentialGrantKeyDomain)
				},
			} {
				derived, err := load(path)
				if err != nil {
					t.Fatal(err)
				}
				if keys := derived.keys.Load(); keys.current.ID != "" || !bytes.Equal(keys.current.Key, want) {
					t.Fatal("derived raw loader changed key semantics")
				}
			}
		})
	}
}
