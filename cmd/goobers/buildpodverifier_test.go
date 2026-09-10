package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/podauth"
)

func TestBuildPodVerifierUnsetKeyUsesRegistry(t *testing.T) {
	verifier, err := buildPodVerifier(&instance.Config{})
	if err != nil {
		t.Fatalf("buildPodVerifier() error = %v", err)
	}
	if verifier == nil {
		t.Fatal("buildPodVerifier() verifier = nil")
	}
	if _, ok := verifier.(*podauth.Registry); !ok {
		t.Fatalf("buildPodVerifier() verifier = %T, want *podauth.Registry", verifier)
	}
}

func TestBuildPodVerifierKeyFileReadFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-key")
	verifier, err := buildPodVerifier(&instance.Config{
		API: instance.APIConfig{PodTokenKeyFile: path},
	})
	if err == nil {
		t.Fatal("buildPodVerifier() error = nil, want read error")
	}
	if verifier != nil {
		t.Fatalf("buildPodVerifier() verifier = %T, want nil", verifier)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("buildPodVerifier() error = %v, want os.ErrNotExist", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("buildPodVerifier() error = %q, want path %q", err, path)
	}
}

func TestBuildPodVerifierInvalidKeyMaterial(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  []byte
	}{
		{name: "empty"},
		{name: "short", key: bytes.Repeat([]byte("k"), podauth.MinSignedKeyBytes-1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "pod-token-key")
			if err := os.WriteFile(path, tc.key, 0o600); err != nil {
				t.Fatal(err)
			}

			verifier, err := buildPodVerifier(&instance.Config{
				API: instance.APIConfig{PodTokenKeyFile: path},
			})
			if err == nil {
				t.Fatal("buildPodVerifier() error = nil, want validation error")
			}
			if verifier != nil {
				t.Fatalf("buildPodVerifier() verifier = %T, want nil", verifier)
			}
			if !strings.Contains(err.Error(), "must be at least") {
				t.Fatalf("buildPodVerifier() error = %q, want minimum-length validation error", err)
			}
		})
	}
}

func TestBuildPodVerifierValidKeyUsesSignedKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pod-token-key")
	if err := os.WriteFile(path, bytes.Repeat([]byte("k"), podauth.MinSignedKeyBytes), 0o600); err != nil {
		t.Fatal(err)
	}

	verifier, err := buildPodVerifier(&instance.Config{
		API: instance.APIConfig{PodTokenKeyFile: path},
	})
	if err != nil {
		t.Fatalf("buildPodVerifier() error = %v", err)
	}
	if verifier == nil {
		t.Fatal("buildPodVerifier() verifier = nil")
	}
	if _, ok := verifier.(*podauth.SignedKey); !ok {
		t.Fatalf("buildPodVerifier() verifier = %T, want *podauth.SignedKey", verifier)
	}
}
