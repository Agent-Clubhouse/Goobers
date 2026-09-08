package imageinputs

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxArchiveBytes = 512 << 20
	maxPackageBytes = 1 << 30
	maxPackageFiles = 10000
)

func downloadHarness(ctx context.Context, client *http.Client, pin harnessPin, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pin.url, nil)
	if err != nil {
		return err
	}
	if req.URL.Scheme != "https" || req.URL.User != nil {
		return fmt.Errorf("harness input must use HTTPS without credentials")
	}
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download harness: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("harness download returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > maxArchiveBytes {
		return fmt.Errorf("harness archive exceeds size limit")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	hash := sha512.New()
	n, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, maxArchiveBytes+1))
	if err != nil {
		return err
	}
	if n > maxArchiveBytes {
		return fmt.Errorf("harness archive exceeds size limit")
	}
	if hex.EncodeToString(hash.Sum(nil)) != pin.digest {
		return fmt.Errorf("harness archive SHA512 mismatch")
	}
	return file.Close()
}

func unpackHarness(archive, directory string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	reader := tar.NewReader(gz)
	var total int64
	for count := 0; ; count++ {
		h, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if count >= maxPackageFiles || h.Size < 0 || h.Size > maxPackageBytes-total {
			return fmt.Errorf("harness package exceeds extraction limits")
		}
		total += h.Size
		if err := unpackEntry(reader, h, directory); err != nil {
			return err
		}
	}
}

func unpackEntry(r io.Reader, h *tar.Header, directory string) error {
	name, ok := strings.CutPrefix(h.Name, "package/")
	name = strings.TrimSuffix(name, "/")
	if !ok || !fs.ValidPath(name) || strings.ContainsAny(name, "\\:") || name == "." {
		return fmt.Errorf("invalid harness archive path %q", h.Name)
	}
	path := filepath.Join(directory, filepath.FromSlash(name))
	if h.Typeflag == tar.TypeDir {
		return os.MkdirAll(path, 0o755)
	}
	if h.Typeflag != tar.TypeReg || h.Mode&0o6000 != 0 {
		return fmt.Errorf("harness archive contains a link, device or privileged file")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o444)
	if h.Mode&0o111 != 0 {
		mode = 0o555
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := io.CopyN(f, r, h.Size); err != nil {
		return err
	}
	return f.Close()
}
