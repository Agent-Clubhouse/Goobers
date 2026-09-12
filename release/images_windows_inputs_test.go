package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"debug/buildinfo"
	"debug/pe"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func imageDependencyFor(url string, data []byte) imageDependency {
	digest := sha256.Sum256(data)
	return imageDependency{URL: url, SHA256: hex.EncodeToString(digest[:])}
}

func useWindowsImageFixtures(t *testing.T, corrupt bool) {
	t.Helper()
	files, err := readWindowsImageMaterials("..")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/mingit", func(w http.ResponseWriter, _ *http.Request) {
		data := "pinned mingit fixture"
		if corrupt {
			data = "corrupted upstream fixture"
		}
		_, _ = io.WriteString(w, data)
	})
	mux.HandleFunc("/zoneinfo", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "pinned zoneinfo fixture") })
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)
	client := *windowsImageHTTPClient
	client.Transport = server.Client().Transport
	originalClient, originalSource := windowsImageHTTPClient, windowsImageSourceDirectory
	windowsImageHTTPClient, windowsImageSourceDirectory = &client, t.TempDir()
	t.Cleanup(func() { windowsImageHTTPClient, windowsImageSourceDirectory = originalClient, originalSource })
	dependencies, err := parseWindowsImageDependencies(files["dependencies.json"])
	if err != nil {
		t.Fatal(err)
	}
	dependencies.MinGit = imageDependencyFor(server.URL+"/mingit", []byte("pinned mingit fixture"))
	dependencies.ZoneInfo = imageDependencyFor(server.URL+"/zoneinfo", []byte("pinned zoneinfo fixture"))
	files["dependencies.json"], err = json.Marshal(dependencies)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(windowsImageSourceDirectory, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReleasePreparesWindowsImageContextFromArchive(t *testing.T) {
	useImageTestBinaries(t)
	useWindowsImageFixtures(t, false)
	root := t.TempDir()
	args := append(imageTestArgs(root), "-targets", "linux/arm64,windows/amd64")
	if err := run(args, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "images", "windows-amd64")
	archive, err := zip.OpenReader(filepath.Join(root, "assets", "goobers_v0.4.0-rc.1_windows_amd64.zip"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = archive.Close() }()
	archived, err := archive.Open("goobers.exe")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = archived.Close() }()
	archiveBytes, err := io.ReadAll(archived)
	if err != nil {
		t.Fatal(err)
	}
	imageBytes, err := os.ReadFile(filepath.Join(directory, "goobers.exe"))
	if err != nil || !bytes.Equal(imageBytes, archiveBytes) {
		t.Fatalf("Windows image binary differs from archive: %v", err)
	}
	for _, name := range []string{"goobers.exe", "goobers-operator.exe"} {
		assertWindowsImageBinary(t, filepath.Join(directory, name))
	}
	data, err := os.ReadFile(filepath.Join(directory, "release.json"))
	if err != nil {
		t.Fatal(err)
	}
	var metadata imageContextMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatal(err)
	}
	want := imageContextMetadata{1, "goobers-base-build-inputs", "v0.4.0-rc.1", "0123456789abcdef0123456789abcdef01234567", "2026-09-07T12:00:00Z", "windows/amd64"}
	if metadata != want {
		t.Fatalf("metadata = %+v, want %+v", metadata, want)
	}
	for _, name := range windowsImageSourceFiles {
		original, err := os.ReadFile(filepath.Join(windowsImageSourceDirectory, name))
		if err != nil {
			t.Fatal(err)
		}
		prepared, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil || !bytes.Equal(prepared, original) {
			t.Fatalf("Windows source %s differs: %v", name, err)
		}
	}
	assertWindowsImageChecksums(t, directory)
	if _, err := os.Stat(filepath.Join(root, "images", "linux-arm64", "goobers")); err != nil {
		t.Fatalf("mixed batch lost Linux context: %v", err)
	}
	assertNoImageStaging(t, root)
}

func assertWindowsImageBinary(t *testing.T, path string) {
	t.Helper()
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	settings := make(map[string]string)
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	if settings["GOOS"] != "windows" || settings["GOARCH"] != "amd64" || settings["CGO_ENABLED"] != "0" {
		t.Fatalf("unexpected Windows build inputs: %v", settings)
	}
	file, err := pe.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if file.Machine != pe.IMAGE_FILE_MACHINE_AMD64 {
		t.Fatalf("PE machine = %d", file.Machine)
	}
	data, err := file.Section(".rdata").Data()
	if err != nil {
		t.Fatal(err)
	}
	for _, stamp := range []string{"v0.4.0-rc.1", "0123456789abcdef0123456789abcdef01234567", "2026-09-07T12:00:00Z"} {
		if !bytes.Contains(data, []byte(stamp)) {
			t.Fatalf("%s missing linked build stamp %s", path, stamp)
		}
	}
}

func assertWindowsImageChecksums(t *testing.T, directory string) {
	t.Helper()
	var paths []string
	names := append(append([]string(nil), windowsImageSourceFiles...), "goobers.exe", "goobers-operator.exe", "release.json", "mingit.zip", "zoneinfo.zip")
	for _, name := range names {
		paths = append(paths, filepath.Join(directory, name))
	}
	want, err := checksumsManifest(paths)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(directory, "SHA256SUMS"))
	if err != nil || string(got) != want {
		t.Fatalf("Windows context checksum mismatch: %v", err)
	}
}

func TestWindowsDependencyFailureDiscardsWholeImageBatch(t *testing.T) {
	useImageTestBinaries(t)
	useWindowsImageFixtures(t, true)
	root := t.TempDir()
	err := run(append(imageTestArgs(root), "-targets", "linux/arm64,windows/amd64"), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "mingit.zip SHA256 mismatch") {
		t.Fatalf("tampered dependency error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "images")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed Windows download exposed partial batch: %v", err)
	}
	assertNoImageStaging(t, root)
}

func TestDownloadImageDependencyRejectsUnsafeOrInvalidInputs(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		limit      int64
		badPin     bool
	}{
		{"HTTP failure", "", http.StatusForbidden, 64, false},
		{"wrong digest", "tampered", http.StatusOK, 64, false},
		{"oversized body", "more than limit", http.StatusOK, 1, false},
		{"malformed digest", "pinned", http.StatusOK, 64, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			dependency := imageDependencyFor(server.URL, []byte("pinned"))
			if tc.badPin {
				dependency.SHA256 = "not-a-digest"
			}
			directory := t.TempDir()
			if err := downloadImageDependency(server.Client(), directory, "test.zip", dependency, tc.limit); err == nil {
				t.Fatal("invalid dependency accepted")
			}
			if _, err := os.Stat(filepath.Join(directory, "test.zip")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid dependency was written: %v", err)
			}
		})
	}
	for _, source := range []string{"http://example.com/archive.zip", "https://token@example.com/archive.zip", "https://example.com/archive.zip?token=secret", "https://example.com/archive.zip#fragment"} {
		if err := validateImageDependency(imageDependencyFor(source, []byte("pinned"))); err == nil {
			t.Fatalf("unsafe source accepted: %s", source)
		}
	}
}

func TestWindowsDependencyRedirectCannotDowngradeHTTPS(t *testing.T) {
	var contacted atomic.Bool
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		contacted.Store(true)
		_, _ = io.WriteString(w, "pinned")
	}))
	defer plain.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL, http.StatusFound)
	}))
	defer secure.Close()
	client := *windowsImageHTTPClient
	client.Transport = secure.Client().Transport
	err := downloadImageDependency(&client, t.TempDir(), "test.zip", imageDependencyFor(secure.URL, []byte("pinned")), 64)
	if err == nil || contacted.Load() {
		t.Fatalf("HTTPS downgrade was not refused: error=%v, insecure server contacted=%t", err, contacted.Load())
	}
}

func TestImageDependencyDownloadTimeoutLeavesNoInput(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer server.Close()
	client := server.Client()
	client.Timeout = 50 * time.Millisecond
	directory := t.TempDir()
	err := downloadImageDependency(client, directory, "test.zip", imageDependencyFor(server.URL, []byte("pinned")), 64)
	if err == nil {
		t.Fatal("stalled download did not time out")
	}
	if _, err := os.Stat(filepath.Join(directory, "test.zip")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stalled download left an input: %v", err)
	}
}

func TestWindowsDependencyPinsRejectTrailingJSON(t *testing.T) {
	files, err := readWindowsImageMaterials("..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseWindowsImageDependencies(append(files["dependencies.json"], []byte("\n{}")...)); err == nil {
		t.Fatal("ambiguous dependency manifest accepted")
	}
}
