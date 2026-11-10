package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cloudauth "github.com/goobers/goobers/internal/cloud/podauth"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/podauth"
)

func writeWorkerRing(t *testing.T, path string, current podauth.SigningKey, previous *podauth.SigningKey) {
	t.Helper()
	raw, err := json.Marshal(struct {
		Current  podauth.SigningKey  `json:"current"`
		Previous *podauth.SigningKey `json:"previous,omitempty"`
	}{current, previous})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

// Exercise both file-loading entry points and every token family used by the
// worker's shared minter, with the real control authenticator on the other side.
func workerSigningTokens(t *testing.T, rawPath, ringPath string) map[string]string {
	t.Helper()
	config, err := workerConfigPlaneTokenSource(rawPath, ringPath)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := podTokenMinter(&instance.Config{API: instance.APIConfig{PodTokenKeyFile: rawPath, PodTokenKeyRingFile: ringPath}})
	if err != nil {
		t.Fatal(err)
	}
	tokens := map[string]string{}
	add := func(name string, mint func() (string, error)) {
		t.Helper()
		token, err := mint()
		if err != nil {
			t.Fatal(err)
		}
		tokens[name] = token
	}
	add("config-fetch", config)
	for name, mint := range map[string]func(string, time.Duration) (string, error){
		"dispatch":          signer.Mint,
		"blob":              signer.MintWorkerBlob,
		"surrender":         signer.MintWorkerSurrender,
		"terminal":          signer.MintWorkerTerminalIntervention,
		"child-admission":   signer.MintWorkerChildAdmission,
		"config-divergence": signer.MintWorkerConfigDigest,
	} {
		add(name, func() (string, error) { return mint("worker-run", time.Minute) })
	}
	add("journal", func() (string, error) { return signer.MintScoped("worker-run", time.Minute, podauth.ScopeJournal) })
	return tokens
}

func authenticateWorkerToken(auth httpapi.Authenticator, token string) error {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	_, err := auth.Authenticate(request)
	return err
}

func TestWorkerKeyRingAuthenticatesAndRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keyring.json")
	old := podauth.SigningKey{ID: "old", Key: bytes.Repeat([]byte("o"), 32)}
	next := podauth.SigningKey{ID: "next", Key: bytes.Repeat([]byte("n"), 32)}
	writeWorkerRing(t, path, old, &next)
	auth, err := cloudauth.New(cloudauth.Config{Mode: "signed", KeyFile: path, Revocations: admitAllGrants{}, Fallback: httpapi.DenyAllAuthenticator{}})
	if err != nil {
		t.Fatal(err)
	}
	oldTokens := workerSigningTokens(t, "", path)
	for name, token := range oldTokens {
		if !strings.Contains(token, ".kid:old.") {
			t.Fatalf("%s did not sign with current key ID", name)
		}
		if err := authenticateWorkerToken(auth, token); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// A restarted worker promotes first; control still accepts its new current
	// key through the pre-distributed previous-key verification window.
	writeWorkerRing(t, path, next, &old)
	newTokens := workerSigningTokens(t, "", path)
	for name, token := range newTokens {
		if !strings.Contains(token, ".kid:next.") {
			t.Fatalf("%s did not sign with promoted key ID", name)
		}
		if err := authenticateWorkerToken(auth, token); err != nil {
			t.Fatalf("pre-promotion %s: %v", name, err)
		}
	}
	if err := auth.PodKeys.ReloadKeyFile(); err != nil {
		t.Fatal(err)
	}
	for _, tokens := range []map[string]string{oldTokens, newTokens} {
		for name, token := range tokens {
			if err := authenticateWorkerToken(auth, token); err != nil {
				t.Fatalf("overlap %s: %v", name, err)
			}
		}
	}
	writeWorkerRing(t, path, next, nil)
	if err := auth.PodKeys.ReloadKeyFile(); err != nil {
		t.Fatal(err)
	}
	for name, token := range oldTokens {
		if err := authenticateWorkerToken(auth, token); err == nil {
			t.Fatalf("retired %s key accepted", name)
		}
	}
	for name, token := range newTokens {
		if err := authenticateWorkerToken(auth, token); err != nil {
			t.Fatalf("current %s: %v", name, err)
		}
	}
}

func TestWorkerRawSigningFilesKeepLegacySemantics(t *testing.T) {
	for name, raw := range map[string][]byte{
		"raw":                  bytes.Repeat([]byte("k"), 32),
		"brace-prefixed":       append([]byte("{"), bytes.Repeat([]byte("k"), 63)...),
		"json":                 []byte(`{"current":{"id":"raw","key":"this-is-still-raw-key-material"}}`),
		"over-ring-size-limit": bytes.Repeat([]byte("k"), 64*1024+1),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "key")
			if err := os.WriteFile(path, append(append([]byte(" \n"), raw...), '\n'), 0600); err != nil {
				t.Fatal(err)
			}
			key, err := podauth.LoadSignedKeyFile(path)
			if err != nil {
				t.Fatal(err)
			}
			auth, err := podauth.NewAuthenticator(key, httpapi.DenyAllAuthenticator{})
			if err != nil {
				t.Fatal(err)
			}
			for name, token := range workerSigningTokens(t, path, "") {
				if strings.Contains(token, ".kid:") {
					t.Fatalf("%s raw token acquired a key ID", name)
				}
				if err := authenticateWorkerToken(auth, token); err != nil {
					t.Fatalf("%s raw compatibility: %v", name, err)
				}
			}
		})
	}
}

func TestWorkerSigningFilesFailClosed(t *testing.T) {
	for name, input := range map[string]struct{ raw, ring, want string }{
		"both":           {"raw", "ring", "mutually exclusive"},
		"malformed-json": {"", `{"current":`, "invalid signing key ring JSON"},
		"invalid-base64": {"", `{"current":{"id":"v1","key":"!"}}`, "invalid signing key ring JSON"},
		"weak-ring":      {"", `{"current":{"id":"v1","key":"aw=="}}`, "at least 32 bytes"},
		"raw-in-ring":    {"", strings.Repeat("k", 32), "invalid signing key ring JSON"},
		"short-raw":      {"short", "", "at least 32 bytes"},
	} {
		t.Run(name, func(t *testing.T) {
			var rawPath, ringPath string
			if input.raw != "" {
				rawPath = filepath.Join(t.TempDir(), "key")
				if err := os.WriteFile(rawPath, []byte(input.raw), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if input.ring != "" {
				ringPath = filepath.Join(t.TempDir(), "keyring.json")
				if err := os.WriteFile(ringPath, []byte(input.ring), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if source, err := workerConfigPlaneTokenSource(rawPath, ringPath); err == nil || !strings.Contains(err.Error(), input.want) || source != nil {
				t.Fatalf("config source = %v, error = %v; want %s", source != nil, err, input.want)
			}
			if signer, err := podTokenMinter(&instance.Config{API: instance.APIConfig{PodTokenKeyFile: rawPath, PodTokenKeyRingFile: ringPath}}); err == nil || !strings.Contains(err.Error(), input.want) || signer != nil {
				t.Fatalf("pod signer = %v, error = %v; want %s", signer != nil, err, input.want)
			}
		})
	}
}

func TestWorkerRejectsBothConfigKeyFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runWorker([]string{"--config-key-file", "raw", "--config-keyring-file", "ring"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "mutually exclusive") {
		t.Fatalf("exit %d: %s", code, &stderr)
	}
}
