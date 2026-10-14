package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/podauth"
)

func TestUpKeepsLoopbackPodAuthorityScoped(t *testing.T) {
	keyBytes := []byte(strings.Repeat("q", 32))
	keyPath := filepath.Join(t.TempDir(), "pod.key")
	if err := os.WriteFile(keyPath, keyBytes, 0600); err != nil {
		t.Fatal(err)
	}
	daemon := startUpOnFreeLoopback(t, freeLoopbackAddress, func(address string) string {
		root := initDeterministicDemo(t)
		layout := instance.NewLayout(root)
		cfg, err := instance.LoadConfig(layout.ConfigFile())
		if err != nil {
			t.Fatal(err)
		}
		cfg.API.Listen = address
		cfg.API.PodTokenKeyFile = keyPath
		if err := instance.WriteConfig(layout.ConfigFile(), cfg); err != nil {
			t.Fatal(err)
		}
		return root
	})
	t.Cleanup(func() {
		daemon.cancel()
		select {
		case code := <-daemon.done:
			if code != 0 {
				t.Errorf("daemon exit %d: %s", code, daemon.stderr.String())
			}
		case <-time.After(15 * time.Second):
			t.Error("daemon failed to stop")
		}
	})
	key, err := podauth.NewSignedKey(keyBytes)
	if err != nil {
		t.Fatal(err)
	}
	token, err := key.MintScoped(strings.Repeat("a", 32), time.Minute, httpapi.ScopeBlob)
	if err != nil {
		t.Fatal(err)
	}
	blob := strings.Replace(apicontract.BlobDigestPath, "{digest}", "sha256:"+strings.Repeat("a", 64), 1)
	cases := []struct {
		name, path, token string
		status            int
	}{
		{"anonymous local health", httpapi.HealthPath, "", http.StatusOK},
		{"anonymous cannot impersonate pod", blob, "", http.StatusForbidden},
		{"valid scoped pod reaches blob lookup", blob, token, http.StatusNotFound},
		{"pod scope cannot read admin surface", httpapi.HealthPath, token, http.StatusForbidden},
		{"invalid signed token cannot become admin", httpapi.HealthPath, token + "corrupted", http.StatusUnauthorized},
		{"unknown token cannot become admin", httpapi.HealthPath, "unrecognized", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+daemon.address+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			response, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if response.StatusCode != tc.status {
				t.Fatalf("HTTP %d, want %d", response.StatusCode, tc.status)
			}
		})
	}
}
