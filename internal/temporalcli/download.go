// Package temporalcli acquires the pinned Temporal CLI used by dev-server tests.
package temporalcli

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	// EnvVar names the offline override for a pre-existing Temporal CLI.
	EnvVar = "GOOBERS_TEMPORAL_CLI"
	// Version is the Temporal CLI release used by dev-server tests.
	Version = "v1.9.0"

	maxAttempts   = 4
	baseDelay     = 250 * time.Millisecond
	maxDelay      = 4 * time.Second
	maxRetryAfter = 30 * time.Second
)

type downloader struct {
	client  *http.Client
	now     func() time.Time
	sleep   func(context.Context, time.Duration) error
	jitter  func(time.Duration) time.Duration
	infoURL string
}

func newDownloader() *downloader {
	return &downloader{
		client: http.DefaultClient,
		now:    time.Now,
		sleep: func(ctx context.Context, delay time.Duration) error {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
		jitter: func(limit time.Duration) time.Duration {
			if limit <= 0 {
				return 0
			}
			return time.Duration(rand.Int63n(int64(limit) + 1))
		},
		infoURL: fmt.Sprintf(
			"https://temporal.download/cli/%s?platform=%s&arch=%s&sdk-name=goobers&sdk-version=%s",
			Version, runtime.GOOS, runtime.GOARCH, Version,
		),
	}
}

// Download fetches the pinned Temporal CLI to output unless it already exists.
func Download(ctx context.Context, output string) error {
	if err := newDownloader().download(ctx, output); err != nil {
		return fmt.Errorf("download Temporal CLI %s: %w; set %s to a pre-existing Temporal CLI binary to run offline",
			Version, err, EnvVar)
	}
	return nil
}

func (d *downloader) download(ctx context.Context, output string) error {
	if _, err := os.Stat(output); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect destination %q: %w", output, err)
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		return fmt.Errorf("unsupported platform %q", runtime.GOOS)
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return fmt.Errorf("unsupported architecture %q", runtime.GOARCH)
	}

	var info struct {
		ArchiveURL    string `json:"archiveUrl"`
		FileToExtract string `json:"fileToExtract"`
	}
	if err := d.get(ctx, d.infoURL, func(body io.Reader) error {
		return json.NewDecoder(io.LimitReader(body, 1<<20)).Decode(&info)
	}); err != nil {
		return fmt.Errorf("resolve archive: %w", err)
	}
	if info.ArchiveURL == "" || info.FileToExtract == "" {
		return errors.New("download metadata omitted archiveUrl or fileToExtract")
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return fmt.Errorf("create destination directory: %w", err)
	}

	archive, err := os.CreateTemp(filepath.Dir(output), "temporal-cli-archive-*")
	if err != nil {
		return fmt.Errorf("create temporary archive: %w", err)
	}
	archivePath := archive.Name()
	if err := archive.Close(); err != nil {
		_ = os.Remove(archivePath)
		return fmt.Errorf("close temporary archive: %w", err)
	}
	defer func() { _ = os.Remove(archivePath) }()

	if err := d.get(ctx, info.ArchiveURL, func(body io.Reader) error {
		file, err := os.OpenFile(archivePath, os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(file, body)
		closeErr := file.Close()
		return errors.Join(copyErr, closeErr)
	}); err != nil {
		return fmt.Errorf("fetch archive: %w", err)
	}

	temp, err := os.CreateTemp(filepath.Dir(output), "temporal-cli-extract-*")
	if err != nil {
		return fmt.Errorf("create temporary executable: %w", err)
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()

	switch {
	case strings.HasSuffix(info.ArchiveURL, ".tar.gz"):
		err = extractTarGZ(archivePath, info.FileToExtract, temp)
	case strings.HasSuffix(info.ArchiveURL, ".zip"):
		err = extractZIP(archivePath, info.FileToExtract, temp)
	default:
		err = fmt.Errorf("unsupported archive URL %q", info.ArchiveURL)
	}
	closeErr := temp.Close()
	if err != nil {
		return fmt.Errorf("extract executable: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("close executable: %w", closeErr)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(tempPath, 0o755); err != nil {
			return fmt.Errorf("make executable: %w", err)
		}
	}
	if err := os.Rename(tempPath, output); err != nil {
		return fmt.Errorf("install executable: %w", err)
	}
	return nil
}

func (d *downloader) get(ctx context.Context, endpoint string, consume func(io.Reader) error) error {
	var lastErr error
	attempts := 0
	for attempt := 0; attempt < maxAttempts; attempt++ {
		attempts = attempt + 1
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		resp, err := d.client.Do(req)
		retry := err != nil && ctx.Err() == nil
		var retryAfter time.Duration
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				err = consume(resp.Body)
				retry = isTransientReadError(err)
			} else {
				err = fmt.Errorf("HTTP %s", resp.Status)
				retry = resp.StatusCode == http.StatusTooManyRequests ||
					(resp.StatusCode >= 500 && resp.StatusCode <= 599)
				retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), d.now())
			}
			closeErr := resp.Body.Close()
			if err == nil {
				err = closeErr
			}
		}
		if err == nil {
			return nil
		}
		lastErr = err
		if !retry || attempt+1 == maxAttempts {
			break
		}
		delay := d.backoff(attempt)
		if retryAfter > delay {
			delay = retryAfter
		}
		if err := d.sleep(ctx, delay); err != nil {
			return err
		}
	}
	return fmt.Errorf("%w (after %d attempt(s))", lastErr, attempts)
}

func (d *downloader) backoff(attempt int) time.Duration {
	delay := min(baseDelay<<attempt, maxDelay)
	half := delay / 2
	return half + d.jitter(half)
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return min(time.Duration(seconds)*time.Second, maxRetryAfter)
	}
	if retryAt, err := http.ParseTime(value); err == nil && retryAt.After(now) {
		return min(retryAt.Sub(now), maxRetryAfter)
	}
	return 0
}

func isTransientReadError(err error) bool {
	var networkError net.Error
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.As(err, &networkError)
}

func extractTarGZ(archivePath, wanted string, output io.Writer) (retErr error) {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, file.Close()) }()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, gz.Close()) }()
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%q not found in archive", wanted)
		}
		if err != nil {
			return err
		}
		if header.Name == wanted || filepath.Base(header.Name) == wanted {
			_, err = io.Copy(output, reader)
			return err
		}
	}
}

func extractZIP(archivePath, wanted string, output io.Writer) (retErr error) {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, reader.Close()) }()
	for _, file := range reader.File {
		if file.Name != wanted && filepath.Base(file.Name) != wanted {
			continue
		}
		source, err := file.Open()
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(output, source)
		return errors.Join(copyErr, source.Close())
	}
	return fmt.Errorf("%q not found in archive", wanted)
}
