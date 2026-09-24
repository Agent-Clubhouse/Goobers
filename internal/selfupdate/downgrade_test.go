package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPrepareManualDowngradeRequiresOptInAndKeepsValidation(t *testing.T) {
	archive := testTarGz(t, "candidate")
	sum := sha256.Sum256(archive)
	var server *httptest.Server
	var badChecksum atomic.Bool
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/goobers/releases/tags/v0.4.1":
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v0.4.1", "assets": []map[string]string{
				{"name": "goobers_v0.4.1_linux_amd64.tar.gz", "browser_download_url": server.URL + "/archive"},
				{"name": "SHA256SUMS", "browser_download_url": server.URL + "/sums"},
			}})
		case "/archive":
			_, _ = w.Write(archive)
		case "/sums":
			checksum := sum
			if badChecksum.Load() {
				checksum[0] ^= 1
			}
			_, _ = fmt.Fprintf(w, "%x  goobers_v0.4.1_linux_amd64.tar.gz\n", checksum)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	for _, test := range []struct {
		name, current string
		allow, want   bool
		badChecksum   bool
	}{
		{"default preserves newer", "v0.5.0-beta.1", false, false, false},
		{"explicit downgrade", "v0.5.0-beta.1", true, true, false},
		{"same version is no-op", "v0.4.1", true, false, false},
		{"forward upgrade unchanged", "v0.4.0", true, true, false},
		{"downgrade rejects corrupt payload", "v0.5.0-beta.1", true, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			badChecksum.Store(test.badChecksum)
			root := t.TempDir()
			current := currentBinary(root, "linux")
			writeTestExecutable(t, current, "current")
			smokes := 0
			runner := commandFunc(func(_ context.Context, _ string, _ []string, name string, args ...string) ([]byte, error) {
				if name == current {
					return []byte(fmt.Sprintf(`{"version":%q,"commit":"current"}`, test.current)), nil
				}
				smokes++
				if len(args) > 0 && args[0] == "version" {
					return []byte(`{"version":"v0.4.1","commit":"candidate"}`), nil
				}
				return nil, nil
			})
			result, err := Prepare(context.Background(), PrepareOptions{
				Root: root, WorkDir: t.TempDir(), Policy: PolicyManual, Target: "v0.4.1",
				AllowDowngrade: test.allow, Owner: "acme", Repository: "goobers",
				GOOS: "linux", GOARCH: "amd64", APIBaseURL: server.URL, HTTPClient: server.Client(),
				Runner: runner, HealthTicks: 1, HealthTimeout: time.Minute, HeartbeatInterval: time.Second,
			})
			if test.badChecksum {
				if err == nil || !strings.Contains(err.Error(), "checksum") {
					t.Fatalf("error=%v, want checksum failure", err)
				}
				if _, err := os.Stat(requestPath(root)); !os.IsNotExist(err) {
					t.Fatalf("handoff exists after integrity failure: %v", err)
				}
				if smokes != 0 {
					t.Fatalf("smoke-tested unverified downgrade: %d", smokes)
				}
				return
			}
			if err != nil || result.UpdateRequested != test.want {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			if test.want {
				request, err := readRequest(root)
				if err != nil || request.Target != "v0.4.1" || request.Status != "requested" || smokes != 3 {
					t.Fatalf("request=%+v smokes=%d error=%v", request, smokes, err)
				}
			} else if smokes != 0 {
				t.Fatalf("no-op smoke checks=%d", smokes)
			}
		})
	}
}

func TestPrepareRejectsAutomaticDowngradeOverride(t *testing.T) {
	for _, policy := range []string{PolicyOnRelease, PolicyOnMain} {
		_, err := Prepare(context.Background(), PrepareOptions{Policy: policy, AllowDowngrade: true})
		if err == nil || !strings.Contains(err.Error(), "requires manual policy") {
			t.Fatalf("policy=%s error=%v", policy, err)
		}
	}
}
