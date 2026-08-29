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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type commandFunc func(context.Context, string, []string, string, ...string) ([]byte, error)

func (f commandFunc) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	return f(ctx, dir, env, name, args...)
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
			current := currentBinary(root, "linux")
			writeTestExecutable(t, current, "old")
			smokes := 0
			runner := commandFunc(func(_ context.Context, _ string, env []string, name string, args ...string) ([]byte, error) {
				if name == current {
					return []byte(`{"version":"v1","commit":"old"}`), nil
				}
				if name == "git" && args[0] == "clone" {
					if args[len(args)-2] != "https://github.com/acme/goobers.git" {
						return nil, fmt.Errorf("clone source = %q, want configured repository", args[len(args)-2])
					}
					environment := strings.Join(env, "\n")
					if !strings.Contains(environment, "GIT_ASKPASS=") ||
						!strings.Contains(environment, "GOOBERS_GIT_TOKEN=token") ||
						!strings.Contains(environment, "GIT_TERMINAL_PROMPT=0") ||
						strings.Contains(strings.Join(args, "\n"), "token") {
						return nil, errors.New("private clone did not use configured-token askpass authentication")
					}
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
			request, err := readRequest(root)
			if err != nil {
				t.Fatal(err)
			}
			if !result.UpdateRequested || request.Policy != policy || request.Status != "requested" || smokes != 3 {
				t.Fatalf("result = %+v, request = %+v, smokes = %d", result, request, smokes)
			}
		})
	}
}

func TestPrepareReleaseFailuresLeaveCurrentBinaryUntouched(t *testing.T) {
	const archiveName = "goobers_v2_linux_amd64.tar.gz"
	tests := []struct {
		name           string
		archive        func(*testing.T) []byte
		manifest       func([]byte) string
		serveArchive   func(http.ResponseWriter, []byte)
		setupCollision bool
		wantError      string
	}{
		{
			name:     "truncated download",
			archive:  func(t *testing.T) []byte { return testTarGz(t, "release") },
			manifest: releaseManifest,
			serveArchive: func(w http.ResponseWriter, archive []byte) {
				w.Header().Set("Content-Length", strconv.Itoa(len(archive)))
				_, _ = w.Write(archive[:len(archive)/2])
			},
			wantError: "unexpected EOF",
		},
		{
			name:      "bad checksum",
			archive:   func(t *testing.T) []byte { return testTarGz(t, "release") },
			manifest:  func([]byte) string { return strings.Repeat("0", sha256.Size*2) + "  " + archiveName + "\n" },
			wantError: "checksum mismatch",
		},
		{
			name:      "partial binary write",
			archive:   testPartialTarGz,
			manifest:  releaseManifest,
			wantError: "unexpected EOF",
		},
		{
			name:           "staging directory collision",
			archive:        func(t *testing.T) []byte { return testTarGz(t, "release") },
			manifest:       releaseManifest,
			setupCollision: true,
			wantError:      "clear staging directory",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, work := t.TempDir(), t.TempDir()
			current := currentBinary(root, "linux")
			writeTestExecutable(t, current, "current release")
			before, err := os.Stat(current)
			if err != nil {
				t.Fatal(err)
			}
			if test.setupCollision {
				if err := os.WriteFile(stagingDir(root), []byte("collision"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			archive := test.archive(t)
			server := newReleaseFixture(t, archive, test.manifest(archive), test.serveArchive)
			runner := commandFunc(func(_ context.Context, _ string, _ []string, name string, _ ...string) ([]byte, error) {
				if name == current {
					return []byte(`{"version":"v1","commit":"current"}`), nil
				}
				return nil, fmt.Errorf("unexpected command %q", name)
			})

			_, err = Prepare(context.Background(), PrepareOptions{
				Root: root, WorkDir: work, Policy: PolicyOnRelease, Owner: "acme", Repository: "goobers",
				RunID: "run", GOOS: "linux", GOARCH: "amd64", APIBaseURL: server.URL,
				HTTPClient: server.Client(), Runner: runner, HealthTicks: 1,
				HealthTimeout: time.Minute, HeartbeatInterval: time.Second,
			})
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("Prepare() error = %v, want error containing %q", err, test.wantError)
			}

			after, err := os.Stat(current)
			if err != nil {
				t.Fatalf("stat current binary after failed update: %v", err)
			}
			raw, err := os.ReadFile(current)
			if err != nil {
				t.Fatalf("read current binary after failed update: %v", err)
			}
			if !os.SameFile(before, after) || string(raw) != "current release" ||
				after.Mode() != before.Mode() || !after.ModTime().Equal(before.ModTime()) {
				t.Fatalf("current binary changed after failed update: same file = %t, content = %q, mode = %v (want %v), modtime = %v (want %v)",
					os.SameFile(before, after), raw, after.Mode(), before.Mode(), after.ModTime(), before.ModTime())
			}
			if _, err := os.Stat(requestPath(root)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("update request exists after failed update: %v", err)
			}
			if !test.setupCollision {
				staged := filepath.Join(stagingDir(root), digestName("v2"), binaryName("linux"))
				if _, err := os.Stat(staged); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("candidate binary exists after failed update: %v", err)
				}
			}
		})
	}
}

func newReleaseFixture(t *testing.T, archive []byte, manifest string, serveArchive func(http.ResponseWriter, []byte)) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/goobers/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v2", "assets": []map[string]string{
				{"name": "goobers_v2_linux_amd64.tar.gz", "browser_download_url": server.URL + "/archive"},
				{"name": "SHA256SUMS", "browser_download_url": server.URL + "/sums"},
			}})
		case "/archive":
			if serveArchive != nil {
				serveArchive(w, archive)
				return
			}
			_, _ = w.Write(archive)
		case "/sums":
			_, _ = io.WriteString(w, manifest)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func releaseManifest(archive []byte) string {
	return fmt.Sprintf("%x  goobers_v2_linux_amd64.tar.gz\n", sha256.Sum256(archive))
}

func testPartialTarGz(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	const content = "partial"
	if err := tarWriter.WriteHeader(&tar.Header{Name: "goobers", Mode: 0o755, Size: int64(len(content) * 2)}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err == nil {
		t.Fatal("closing deliberately partial tar unexpectedly succeeded")
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
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
