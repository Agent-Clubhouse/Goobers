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
	"strings"
	"testing"
	"time"

	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

type commandFunc func(context.Context, string, string, ...string) ([]byte, error)

func (f commandFunc) Run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	return f(ctx, dir, name, args...)
}

func TestPrepareOnReleaseVerifiesSmokesAndRequestsHandoff(t *testing.T) {
	root := t.TempDir()
	current := CurrentBinary(root, "linux")
	writeTestExecutable(t, current, []byte("old"))
	archive := testTarGz(t, "candidate")
	sum := sha256.Sum256(archive)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer release-token" {
			t.Errorf("authorization = %q", got)
		}
		switch r.URL.Path {
		case "/repos/acme/goobers/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1.2.3",
				"assets": []map[string]string{
					{"name": "goobers_v1.2.3_linux_amd64.tar.gz", "browser_download_url": server.URL + "/archive"},
					{"name": "SHA256SUMS", "browser_download_url": server.URL + "/sums"},
				},
			})
		case "/archive":
			_, _ = w.Write(archive)
		case "/sums":
			_, _ = fmt.Fprintf(w, "%x  goobers_v1.2.3_linux_amd64.tar.gz\n", sum)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var smoke [][]string
	workDir := t.TempDir()
	canonicalRoot := filepath.Join(workDir, "selfhost")
	if err := os.MkdirAll(canonicalRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	runner := commandFunc(func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
		call := append([]string{name}, args...)
		smoke = append(smoke, call)
		if name == current {
			return []byte(`{"version":"v1.2.2","commit":"old"}`), nil
		}
		if len(args) == 2 && args[0] == "version" {
			return []byte(`{"version":"v1.2.3","commit":"release"}`), nil
		}
		return nil, nil
	})
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	result, err := Prepare(context.Background(), PrepareOptions{
		Root:              root,
		WorkDir:           workDir,
		Policy:            PolicyOnRelease,
		Owner:             "acme",
		Repository:        "goobers",
		Token:             "release-token",
		RunID:             "run-1",
		HealthTicks:       2,
		HealthTimeout:     time.Minute,
		HeartbeatInterval: 10 * time.Second,
		GOOS:              "linux",
		GOARCH:            "amd64",
		APIBaseURL:        server.URL,
		HTTPClient:        server.Client(),
		Runner:            runner,
		Now:               func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.UpdateRequested || result.Target != "v1.2.3" {
		t.Fatalf("result = %+v", result)
	}
	if got, err := os.ReadFile(result.StagedPath); err != nil || string(got) != "candidate" {
		t.Fatalf("staged binary = %q, %v", got, err)
	}
	request, err := ReadRequest(root)
	if err != nil {
		t.Fatal(err)
	}
	if request.RunID != "run-1" || request.Target != "v1.2.3" || request.HealthTicks != 2 || request.Status != "requested" {
		t.Fatalf("request = %+v", request)
	}
	joined := make([]string, len(smoke))
	for i := range smoke {
		joined[i] = strings.Join(smoke[i], " ")
	}
	for _, want := range []string{"version --json", "validate " + root, "config diff --against " + canonicalRoot + " " + root} {
		found := false
		for _, call := range joined {
			if strings.Contains(call, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("commands %v missing %q", joined, want)
		}
	}
}

func TestPrepareRejectsReleaseChecksumMismatch(t *testing.T) {
	root := t.TempDir()
	current := CurrentBinary(root, "linux")
	writeTestExecutable(t, current, []byte("old"))
	archive := testTarGz(t, "candidate")
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/goobers/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v2",
				"assets": []map[string]string{
					{"name": "goobers_v2_linux_amd64.tar.gz", "browser_download_url": server.URL + "/archive"},
					{"name": "SHA256SUMS", "browser_download_url": server.URL + "/sums"},
				},
			})
		case "/archive":
			_, _ = w.Write(archive)
		case "/sums":
			_, _ = fmt.Fprintln(w, strings.Repeat("0", 64)+"  goobers_v2_linux_amd64.tar.gz")
		}
	}))
	defer server.Close()
	runner := commandFunc(func(_ context.Context, _ string, name string, _ ...string) ([]byte, error) {
		if name == current {
			return []byte(`{"version":"v1","commit":"old"}`), nil
		}
		return nil, nil
	})
	_, err := Prepare(context.Background(), PrepareOptions{
		Root: root, WorkDir: t.TempDir(), Policy: PolicyOnRelease,
		Owner: "acme", Repository: "goobers", GOOS: "linux", GOARCH: "amd64",
		APIBaseURL: server.URL, HTTPClient: server.Client(), Runner: runner,
	})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("Prepare error = %v", err)
	}
	if _, err := os.Stat(RequestPath(root)); !os.IsNotExist(err) {
		t.Fatalf("request exists after checksum failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(StagingDir(root), digestName("v2"))); !os.IsNotExist(err) {
		t.Fatalf("staging exists after checksum failure: %v", err)
	}
}

func TestPrepareRejectsConcurrentPreparationBeforeStaging(t *testing.T) {
	root := t.TempDir()
	writeTestExecutable(t, CurrentBinary(root, "linux"), []byte("old"))
	if err := os.MkdirAll(UpdatesDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	held, err := platformlock.Acquire(filepath.Join(UpdatesDir(root), prepareLockFile))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Release() })

	_, err = Prepare(context.Background(), PrepareOptions{
		Root: root, WorkDir: t.TempDir(), Policy: PolicyManual, Target: "v2",
		Owner: "acme", Repository: "goobers", GOOS: "linux", GOARCH: "amd64",
	})
	if err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("Prepare error = %v", err)
	}
	if _, err := os.Stat(StagingDir(root)); !os.IsNotExist(err) {
		t.Fatalf("staging created during competing preparation: %v", err)
	}
}

func TestStageReleaseCleansStagingAfterDownloadFailure(t *testing.T) {
	root := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "broken download", http.StatusBadGateway)
	}))
	defer server.Close()
	release := githubRelease{TagName: "v2"}
	release.Assets = append(release.Assets,
		struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		}{Name: "goobers_v2_linux_amd64.tar.gz", URL: server.URL + "/archive"},
		struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		}{Name: "SHA256SUMS", URL: server.URL + "/sums"},
	)
	_, err := stageRelease(context.Background(), PrepareOptions{
		Root: root, GOOS: "linux", GOARCH: "amd64", HTTPClient: server.Client(),
	}, release)
	if err == nil || !strings.Contains(err.Error(), "status 502") {
		t.Fatalf("stageRelease error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(StagingDir(root), digestName("v2"))); !os.IsNotExist(err) {
		t.Fatalf("staging exists after download failure: %v", err)
	}
}

func TestPrepareOnMainBuildsConfiguredRepositoryCommit(t *testing.T) {
	root := t.TempDir()
	current := CurrentBinary(root, "linux")
	writeTestExecutable(t, current, []byte("old"))
	const commit = "0123456789abcdef0123456789abcdef01234567"
	sourceArchive := testSourceTarGz(t, map[string]string{"source-marker": "configured repository"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer main-token" {
			t.Errorf("authorization = %q", got)
		}
		switch r.URL.Path {
		case "/repos/acme/goobers/commits/main":
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": commit})
		case "/repos/acme/goobers/tarball/" + commit:
			_, _ = w.Write(sourceArchive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	workDir := t.TempDir()
	runner := commandFunc(func(_ context.Context, dir string, name string, args ...string) ([]byte, error) {
		switch name {
		case current:
			return []byte(`{"version":"dev","commit":"old"}`), nil
		case "go":
			if dir == workDir {
				return nil, errors.New("go build used the invoking worktree")
			}
			marker, err := os.ReadFile(filepath.Join(dir, "source-marker"))
			if err != nil || string(marker) != "configured repository" {
				return nil, fmt.Errorf("build source marker = %q, %v", marker, err)
			}
			for i := range args {
				if args[i] == "-o" && i+1 < len(args) {
					writeTestExecutable(t, args[i+1], []byte("candidate"))
					return nil, nil
				}
			}
			return nil, errors.New("go build omitted -o")
		default:
			if len(args) == 2 && args[0] == "version" {
				return []byte(`{"version":"dev","commit":"` + commit + `"}`), nil
			}
			return nil, nil
		}
	})
	result, err := Prepare(context.Background(), PrepareOptions{
		Root: root, WorkDir: workDir, Policy: PolicyOnMain,
		Owner: "acme", Repository: "goobers", Branch: "main",
		Token: "main-token", GOOS: "linux", GOARCH: "amd64",
		APIBaseURL: server.URL, HTTPClient: server.Client(), Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.UpdateRequested || result.Target != "main@"+commit {
		t.Fatalf("result = %+v", result)
	}
	request, err := ReadRequest(root)
	if err != nil {
		t.Fatal(err)
	}
	if request.Commit != commit || request.Policy != PolicyOnMain {
		t.Fatalf("request = %+v", request)
	}
}

func TestExtractSourceTarGzRejectsBackslashTraversal(t *testing.T) {
	archive := testSourceTarGz(t, map[string]string{`..\..\current\goobers.exe`: "malicious"})
	archivePath := filepath.Join(t.TempDir(), "source.tar.gz")
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := extractSourceTarGz(archivePath, filepath.Join(t.TempDir(), "source")); err == nil ||
		!strings.Contains(err.Error(), "invalid path") {
		t.Fatalf("extractSourceTarGz error = %v", err)
	}
}

func testTarGz(t *testing.T, binary string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{
		Name: "goobers",
		Mode: 0o755,
		Size: int64(len(binary)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write([]byte(binary)); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func testSourceTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	for name, content := range files {
		if err := tarWriter.WriteHeader(&tar.Header{
			Name: "acme-goobers-commit/" + name,
			Mode: 0o644,
			Size: int64(len(content)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func writeTestExecutable(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o755); err != nil {
		t.Fatal(err)
	}
}
