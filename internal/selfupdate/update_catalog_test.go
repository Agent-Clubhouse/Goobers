package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func TestExtractReferenceCatalogRejectsUnsafeEntries(t *testing.T) {
	const prefix = "acme-goobers-abc123/reference-workflows/"
	for _, test := range []struct {
		name   string
		header tar.Header
		want   string
	}{
		{"traversal", tar.Header{Name: prefix + "../escaped.yaml", Typeflag: tar.TypeReg}, "unsafe path"},
		{"symlink", tar.Header{Name: prefix + "linked.yaml", Typeflag: tar.TypeSymlink, Linkname: "../escaped.yaml"}, "unsupported archive entry"},
		{"hardlink", tar.Header{Name: prefix + "linked.yaml", Typeflag: tar.TypeLink, Linkname: prefix + "candidate.yaml"}, "unsupported archive entry"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			archivePath, catalogPath := filepath.Join(root, "source.tar.gz"), filepath.Join(root, "catalog")
			if err := os.WriteFile(archivePath, referenceCatalogWithEntry(t, test.header), 0o600); err != nil {
				t.Fatal(err)
			}
			err := extractReferenceCatalog(archivePath, catalogPath)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("extractReferenceCatalog() error = %v, want %q", err, test.want)
			}
			if content, err := os.ReadFile(filepath.Join(catalogPath, "candidate.yaml")); err != nil || string(content) != "candidate" {
				t.Fatalf("valid entry preceding rejection = %q, %v", content, err)
			}
			for _, path := range []string{filepath.Join(root, "escaped.yaml"), filepath.Join(catalogPath, "linked.yaml")} {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unsafe entry exists at %s: %v", path, err)
				}
			}
		})
	}
}

func TestDownloadReferenceCatalogCleansPartialExtraction(t *testing.T) {
	archive := referenceCatalogWithEntry(t, tar.Header{
		Name: "acme-goobers-abc123/reference-workflows/../escaped.yaml", Typeflag: tar.TypeReg,
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	}))
	defer server.Close()
	stage := t.TempDir()
	binary := filepath.Join(stage, "goobers")
	writeTestExecutable(t, binary, "candidate binary")
	path, _, err := downloadReferenceCatalog(context.Background(), PrepareOptions{
		Owner: "acme", Repository: "goobers", APIBaseURL: server.URL, HTTPClient: server.Client(),
	}, stage, "abc123")
	if err == nil || !strings.Contains(err.Error(), "unsafe path") || path != "" {
		t.Fatalf("downloadReferenceCatalog() = %q, %v, want extraction failure", path, err)
	}
	for _, name := range []string{"reference-source.tar.gz", "reference-workflows", "escaped.yaml"} {
		if _, err := os.Lstat(filepath.Join(stage, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed extraction left %s: %v", name, err)
		}
	}
	if content, err := os.ReadFile(binary); err != nil || string(content) != "candidate binary" {
		t.Fatalf("cleanup changed staged binary: %q, %v", content, err)
	}
}

func TestSmokeCheckPreservesValidationAndDriftFailures(t *testing.T) {
	for _, failedCommand := range []string{"validate", "config"} {
		t.Run(failedCommand, func(t *testing.T) {
			root, work, stage := t.TempDir(), t.TempDir(), t.TempDir()
			binary := filepath.Join(stage, "goobers")
			writeTestExecutable(t, binary, "candidate binary")
			archive := testReferenceTarGz(t, "candidate")
			var downloads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				downloads.Add(1)
				_, _ = w.Write(archive)
			}))
			defer server.Close()
			failure := errors.New("candidate rejected configuration")
			var commands []string
			runner := commandFunc(func(_ context.Context, _ string, _ []string, _ string, args ...string) ([]byte, error) {
				commands = append(commands, args[0])
				switch args[0] {
				case "version":
					return []byte(`{"version":"v1.2.3","commit":"abc123"}`), nil
				case "validate":
					if !slices.Equal(args, []string{"validate", root}) {
						t.Fatalf("validate arguments = %q", args)
					}
				case "config":
					catalog := filepath.Join(stage, "reference-workflows")
					if !slices.Equal(args, []string{"config", "diff", "--against", catalog, root}) {
						t.Fatalf("config diff arguments = %q", args)
					}
					if _, err := os.Stat(filepath.Join(catalog, "candidate.yaml")); err != nil {
						t.Fatalf("catalog missing before config diff: %v", err)
					}
				default:
					return nil, fmt.Errorf("unexpected command %q", args[0])
				}
				if args[0] == failedCommand {
					return nil, failure
				}
				return nil, nil
			})
			_, err := smokeCheck(context.Background(), PrepareOptions{
				Root: root, WorkDir: work, Owner: "acme", Repository: "goobers",
				APIBaseURL: server.URL, HTTPClient: server.Client(), Runner: runner,
			}, binary)
			wantCommands, wantDownloads, wantError := []string{"version", "validate"}, int32(0), "staged validate smoke check"
			if failedCommand == "config" {
				wantCommands, wantDownloads, wantError = append(wantCommands, "config"), 1, "staged config diff smoke check"
			}
			if !errors.Is(err, failure) || !strings.Contains(err.Error(), wantError) {
				t.Fatalf("smokeCheck() error = %v, want wrapped %q failure", err, wantError)
			}
			if !slices.Equal(commands, wantCommands) || downloads.Load() != wantDownloads {
				t.Fatalf("commands = %q, downloads = %d; want %q, %d", commands, downloads.Load(), wantCommands, wantDownloads)
			}
			entries, err := os.ReadDir(stage)
			if err != nil || len(entries) != 1 || entries[0].Name() != "goobers" {
				t.Fatalf("smoke failure left unexpected staged entries: %v, %v", entries, err)
			}
		})
	}
}

func referenceCatalogWithEntry(t *testing.T, extra tar.Header) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	const content = "candidate"
	if err := tarWriter.WriteHeader(&tar.Header{
		Name: "acme-goobers-abc123/reference-workflows/candidate.yaml", Mode: 0o600, Size: int64(len(content)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.WriteHeader(&extra); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(tarWriter.Close(), gzipWriter.Close()); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
