// Package workerblob selects a resident worker's artifact transport and verifies
// directory sharing before a dispatch worker starts polling.
package workerblob

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/dispatcher"
)

// Resolve preserves GOOBERS_BLOB_ENDPOINT's stage-pod meaning when a directory
// is selected. An explicit endpoint always conflicts with a directory.
func Resolve(directory, endpoint, environment string, instanceBacked bool) (string, error) {
	if instanceBacked && endpoint == "" && directory == "" {
		endpoint = environment
	}
	if directory != "" && endpoint != "" || instanceBacked && directory == "" && endpoint == "" {
		return "", fmt.Errorf("WORKER_BLOB_MODE: select exactly one of --blob-store or --blob-endpoint")
	}
	if endpoint != "" {
		if err := ValidateEndpoint(endpoint); err != nil {
			return "", err
		}
	}
	return endpoint, nil
}

// ValidateEndpoint returns credential-free errors before endpoints reach logs.
func ValidateEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") {
		return fmt.Errorf("WORKER_BLOB_ENDPOINT: requires an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("WORKER_BLOB_ENDPOINT: invalid TCP port")
		}
	}
	return nil
}

// Open needs worker authentication only when traffic crosses the blob plane.
func Open(ctx context.Context, directory, endpoint, dispatchEndpoint string, source func() (string, error)) (blobstore.Store, error) {
	if directory == "" {
		if source == nil {
			return nil, fmt.Errorf("WORKER_BLOB_AUTH: api.podTokenKeyFile is required for endpoint mode")
		}
		return &dispatcher.BlobClient{BaseURL: endpoint, TokenSource: source}, nil
	}
	local, err := blobstore.NewDir(directory)
	if err != nil {
		return nil, err
	}
	if dispatchEndpoint != "" {
		if err := ValidateEndpoint(dispatchEndpoint); err != nil {
			return nil, err
		}
		if source == nil {
			return nil, fmt.Errorf("WORKER_BLOB_AUTH: api.podTokenKeyFile is required for dispatch preflight")
		}
		remote := &dispatcher.BlobClient{BaseURL: dispatchEndpoint, TokenSource: source}
		if err := VerifyShared(ctx, local, remote); err != nil {
			return nil, err
		}
	}
	return local, nil
}

// VerifyShared proves a fresh plane write is visible through the worker mount.
// The tiny random content-addressed probe is intentionally retained as a blob.
func VerifyShared(ctx context.Context, local, remote blobstore.Store) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return fmt.Errorf("WORKER_BLOB_STORE_MISMATCH: cannot generate probe")
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(data))
	if err := remote.Put(ctx, digest, data); err != nil {
		return fmt.Errorf("WORKER_BLOB_STORE_MISMATCH: blob-plane probe PUT failed: %w", err)
	}
	got, err := local.Get(ctx, digest)
	if err != nil || !bytes.Equal(got, data) {
		return fmt.Errorf("WORKER_BLOB_STORE_MISMATCH: daemon probe is absent from --blob-store; mount the daemon store or use --blob-endpoint")
	}
	return nil
}
