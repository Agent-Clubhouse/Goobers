package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type commandFunc func(context.Context, string, string, ...string) ([]byte, error)

func (f commandFunc) Run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	return f(ctx, dir, name, args...)
}

func TestPrepareReleaseAndMain(t *testing.T) {
	const commit = "0123456789abcdef0123456789abcdef01234567"
	releaseArchive := testTarGz(t, "release")
	sum := sha256.Sum256(releaseArchive)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/goobers/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v2", "assets": []map[string]string{
				{"name": "goobers_v2_linux_amd64.tar.gz", "browser_download_url": server.URL + "/archive"},
				{"name": "SHA256SUMS", "browser_download_url": server.URL + "/sums"},
			}})
		case "/archive":
			_, _ = w.Write(releaseArchive)
		case "/sums":
			_, _ = fmt.Fprintf(w, "%x  goobers_v2_linux_amd64.tar.gz\n", sum)
		case "/repos/acme/goobers/commits/main":
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": commit})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	for _, policy := range []string{PolicyOnRelease, PolicyOnMain} {
		t.Run(policy, func(t *testing.T) {
			root, work := t.TempDir(), t.TempDir()
			current := CurrentBinary(root, "linux")
			writeTestExecutable(t, current, "old")
			smokes := 0
			runner := commandFunc(func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
				if name == current {
					return []byte(`{"version":"v1","commit":"old"}`), nil
				}
				if name == "git" && args[0] == "clone" && args[len(args)-2] != "https://github.com/acme/goobers.git" {
					return nil, fmt.Errorf("clone source = %q, want configured repository", args[len(args)-2])
				}
				if name == "git" {
					return nil, nil
				}
				if name == "go" {
					for i, arg := range args {
						if arg == "-o" {
							writeTestExecutable(t, args[i+1], "main")
						}
					}
					return nil, nil
				}
				smokes++
				content, _ := os.ReadFile(name)
				if string(content) == "main" {
					return []byte(`{"version":"dev","commit":"` + commit + `"}`), nil
				}
				if len(args) > 0 && args[0] == "version" {
					return []byte(`{"version":"v2","commit":"release"}`), nil
				}
				return nil, nil
			})
			result, err := Prepare(context.Background(), PrepareOptions{
				Root: root, WorkDir: work, Policy: policy, Owner: "acme", Repository: "goobers",
				Branch: "main", Token: "token", RunID: "run", GOOS: "linux", GOARCH: "amd64",
				APIBaseURL: server.URL, HTTPClient: server.Client(), Runner: runner,
				HealthTicks: 1, HealthTimeout: time.Minute, HeartbeatInterval: time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			request, err := ReadRequest(root)
			if err != nil {
				t.Fatal(err)
			}
			if !result.UpdateRequested || request.Policy != policy || request.Status != "requested" || smokes != 3 {
				t.Fatalf("result = %+v, request = %+v, smokes = %d", result, request, smokes)
			}
		})
	}
}

func testTarGz(t *testing.T, content string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "goobers", Mode: 0o755, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(tarWriter.Close(), gzipWriter.Close()); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func writeTestExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}
