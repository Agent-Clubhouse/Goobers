// Package objstore implements cloud content-addressed object storage.
package objstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"

	"github.com/goobers/goobers/internal/blobstore"
)

var (
	// ErrConflict means an existing content address contains different bytes.
	ErrConflict = errors.New("cloud object store create-only conflict")
	// ErrDigestMismatch means downloaded bytes do not match their address.
	ErrDigestMismatch = errors.New("cloud object store digest mismatch")
)

// Azure is a content-addressed blobstore.Store backed by Azure Blob Storage.
type Azure struct {
	client    *azblob.Client
	container string
}

// NewAzure creates a store using an Entra token credential.
func NewAzure(serviceURL, container string, credential azcore.TokenCredential) (*Azure, error) {
	if credential == nil {
		return nil, errors.New("cloud object store credential is required")
	}
	client, err := azblob.NewClient(serviceURL, credential, nil)
	if err != nil {
		return nil, errors.New("cloud object store service URL is invalid")
	}
	return newAzure(client, container)
}

func newAzure(client *azblob.Client, container string) (*Azure, error) {
	if client == nil || strings.TrimSpace(container) == "" {
		return nil, errors.New("cloud object store client and container are required")
	}
	return &Azure{client: client, container: container}, nil
}

// Describe names the container without exposing its account URL or credentials.
func (a *Azure) Describe() string { return "azure-blob:" + a.container }

// Put creates a blob with If-None-Match. An identical existing object is an
// idempotent success; an object whose bytes do not match its key is a conflict.
func (a *Azure) Put(ctx context.Context, digest string, data []byte) error {
	key, err := blobKey(digest)
	if err != nil {
		return err
	}
	if actual := digestFor(data); actual != digest {
		return fmt.Errorf("%w: put %s has digest %s", ErrDigestMismatch, digest, actual)
	}
	_, err = a.client.UploadBuffer(ctx, a.container, key, data, &azblob.UploadBufferOptions{
		AccessConditions: &blob.AccessConditions{
			ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: to.Ptr(azcore.ETagAny)},
		},
	})
	if err == nil {
		return nil
	}
	if !hasStatus(err, 409, 412) {
		return fmt.Errorf("cloud object store put %s: %w", digest, err)
	}
	existing, getErr := a.Get(ctx, digest)
	if getErr == nil && digestFor(existing) == digest {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrConflict, digest)
}

// Get downloads and verifies a content-addressed blob.
func (a *Azure) Get(ctx context.Context, digest string) ([]byte, error) {
	key, err := blobKey(digest)
	if err != nil {
		return nil, err
	}
	response, err := a.client.DownloadStream(ctx, a.container, key, nil)
	if err != nil {
		if hasStatus(err, 404) {
			return nil, fmt.Errorf("%w: %s", blobstore.ErrNotFound, digest)
		}
		return nil, fmt.Errorf("cloud object store get %s: %w", digest, err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("cloud object store read %s: %w", digest, err)
	}
	if digestFor(data) != digest {
		return nil, fmt.Errorf("%w: %s", ErrDigestMismatch, digest)
	}
	return data, nil
}

// Has reports whether a verified object exists.
func (a *Azure) Has(ctx context.Context, digest string) (bool, error) {
	_, err := a.Get(ctx, digest)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, blobstore.ErrNotFound):
		return false, nil
	default:
		return false, err
	}
}

// Remove deletes a blob after its caller proves it unreferenced.
func (a *Azure) Remove(ctx context.Context, digest string) error {
	key, err := blobKey(digest)
	if err != nil {
		return err
	}
	_, err = a.client.DeleteBlob(ctx, a.container, key, nil)
	if err != nil && !hasStatus(err, 404) {
		return fmt.Errorf("cloud object store delete %s: %w", digest, err)
	}
	return nil
}

func blobKey(digest string) (string, error) {
	if !blobstore.ValidDigest(digest) {
		return "", errors.New("cloud object store digest is invalid")
	}
	hex := strings.TrimPrefix(digest, "sha256:")
	return "blobs/sha256/" + hex[:2] + "/" + hex[2:], nil
}

func digestFor(data []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(data))
}

func hasStatus(err error, statuses ...int) bool {
	var responseError *azcore.ResponseError
	if !errors.As(err, &responseError) {
		return false
	}
	for _, status := range statuses {
		if responseError.StatusCode == status {
			return true
		}
	}
	return false
}
