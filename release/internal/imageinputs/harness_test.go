package imageinputs

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixtureArchive(t *testing.T, headers []*tar.Header) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	w := tar.NewWriter(gz)
	for _, h := range headers {
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size < 100 {
			if _, err := w.Write(bytes.Repeat([]byte("x"), int(h.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Oversize fixtures intentionally end after their declared header.
	_ = w.Close()
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func fixturePin(name string, data []byte, url string) harnessPin {
	hash := sha512.Sum512(data)
	return harnessPin{name: name, version: "1.2.3", url: url, digest: hex.EncodeToString(hash[:])}
}

func TestPreparePreservesPackageAndWritesFixedLauncher(t *testing.T) {
	for _, name := range []string{"copilot", "claude"} {
		t.Run(name, func(t *testing.T) {
			data := fixtureArchive(t, []*tar.Header{
				{Name: "package/" + name, Mode: 0o755, Size: 3},
				{Name: "package/LICENSE.md", Mode: 0o644, Size: 5},
				{Name: "package/native/addon.node", Mode: 0o644, Size: 7},
			})
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(data) }))
			defer server.Close()
			out := filepath.Join(t.TempDir(), "context")
			if err := prepareHarness(context.Background(), server.Client(), fixturePin(name, data, server.URL), out); err != nil {
				t.Fatal(err)
			}
			for _, file := range []string{name, "LICENSE.md", "native/addon.node"} {
				if _, err := os.Stat(filepath.Join(out, "package", file)); err != nil {
					t.Fatal(err)
				}
			}
			launcher, err := os.ReadFile(filepath.Join(out, "bin", name))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(launcher), "exec /opt/goobers-harness/"+name+" \"$@\"") {
				t.Fatal("launcher loses caller arguments or immutable executable")
			}
			if name == "copilot" && !strings.Contains(string(launcher), "COPILOT_CLI_DIST_DIR=/opt/goobers-harness") {
				t.Fatal("Copilot would extract native code into noexec HOME")
			}
			if _, err := os.Stat(filepath.Join(out, "harness.tgz")); !os.IsNotExist(err) {
				t.Fatal("downloaded archive remains in final context")
			}
			if err := prepareHarness(context.Background(), server.Client(), fixturePin(name, data, server.URL), out); err == nil {
				t.Fatal("existing context was overwritten")
			}
		})
	}
}

func TestInvalidArchivesNeverPublishContext(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers []*tar.Header
	}{
		{"parent traversal", []*tar.Header{{Name: "package/../escaped", Size: 1}}},
		{"absolute path", []*tar.Header{{Name: "/escaped", Size: 1}}},
		{"windows path", []*tar.Header{{Name: "package/C:\\escaped", Size: 1}}},
		{"symlink", []*tar.Header{{Name: "package/escape", Typeflag: tar.TypeSymlink, Linkname: "../../escaped"}}},
		{"hardlink", []*tar.Header{{Name: "package/escape", Typeflag: tar.TypeLink, Linkname: "/etc/passwd"}}},
		{"device", []*tar.Header{{Name: "package/dev", Typeflag: tar.TypeChar}}},
		{"privileged", []*tar.Header{{Name: "package/copilot", Mode: 0o4755, Size: 1}}},
		{"duplicate", []*tar.Header{{Name: "package/copilot", Mode: 0o755, Size: 1}, {Name: "package/copilot", Mode: 0o755, Size: 1}}},
		{"oversize", []*tar.Header{{Name: "package/copilot", Mode: 0o755, Size: maxPackageBytes + 1}}},
		{"missing executable", []*tar.Header{{Name: "package/copilot", Mode: 0o644, Size: 1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := fixtureArchive(t, tc.headers)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(data) }))
			defer server.Close()
			root := t.TempDir()
			out := filepath.Join(root, "context")
			if err := prepareHarness(context.Background(), server.Client(), fixturePin("copilot", data, server.URL), out); err == nil {
				t.Fatal("accepted invalid package")
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed preparation left files: %v, %v", entries, err)
			}
		})
	}
}

func TestDownloadFailureNeverPublishesContext(t *testing.T) {
	for _, scenario := range []string{"checksum", "HTTP error", "oversize", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			data := fixtureArchive(t, []*tar.Header{{Name: "package/copilot", Mode: 0o755, Size: 1}})
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				switch scenario {
				case "HTTP error":
					w.WriteHeader(http.StatusServiceUnavailable)
				case "oversize":
					w.Header().Set("Content-Length", "536870913")
				default:
					_, _ = w.Write(data)
				}
			}))
			defer server.Close()
			pin := fixturePin("copilot", data, server.URL)
			if scenario == "checksum" {
				pin.digest = strings.Repeat("0", 128)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "cancelled" {
				cancel()
			}
			root := t.TempDir()
			if err := prepareHarness(ctx, server.Client(), pin, filepath.Join(root, "context")); err == nil {
				t.Fatal("download failure accepted")
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed download left staging files: %v, %v", entries, err)
			}
		})
	}
}

func TestDownloadRejectsHTTPSDowngrade(t *testing.T) {
	contacted := false
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		contacted = true
		_, _ = io.WriteString(w, "unexpected")
	}))
	defer plain.Close()
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL, http.StatusFound)
	}))
	defer tls.Close()
	client := harnessClient()
	client.Transport = tls.Client().Transport
	if err := downloadHarness(context.Background(), client, harnessPin{url: tls.URL}, filepath.Join(t.TempDir(), "archive"), maxArchiveBytes); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("downgrade = %v", err)
	}
	if contacted {
		t.Fatal("contacted plaintext redirect destination")
	}
}

func TestUnknownLengthDownloadLimit(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.(http.Flusher).Flush() // Force chunked transfer without Content-Length.
		_, _ = io.WriteString(w, strings.Repeat("x", 33))
	}))
	defer server.Close()
	err := downloadHarness(context.Background(), server.Client(), harnessPin{url: server.URL}, filepath.Join(t.TempDir(), "archive"), 32)
	if err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("unknown-length oversized download = %v", err)
	}
}

func TestCumulativeAndFileCountExtractionLimits(t *testing.T) {
	data := fixtureArchive(t, []*tar.Header{
		{Name: "package/copilot", Mode: 0o755, Size: 20},
		{Name: "package/addon.node", Mode: 0o644, Size: 20},
	})
	archive := filepath.Join(t.TempDir(), "harness.tgz")
	if err := os.WriteFile(archive, data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		bytes int64
		files int
	}{
		{"cumulative bytes", 32, 10},
		{"file count", 100, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := unpackHarness(archive, t.TempDir(), "copilot", tc.bytes, tc.files)
			if err == nil || !strings.Contains(err.Error(), "extraction limits") {
				t.Fatalf("extraction limit = %v", err)
			}
		})
	}
}
