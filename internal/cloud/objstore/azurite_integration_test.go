//go:build cloudintegration

package objstore

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"

	"github.com/goobers/goobers/internal/blobstore"
)

func TestCreateOnlyPutConflict(t *testing.T) {
	store := testAzureStore(t, "goobers-conflict")
	data := []byte("expected content")
	digest := digestFor(data)
	key, err := blobKey(digest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.client.UploadBuffer(t.Context(), store.container, key, []byte("corrupt existing content"), nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(t.Context(), digest, data); !errors.Is(err, ErrConflict) {
		t.Fatalf("Put error = %v, want ErrConflict", err)
	}
	response, err := store.client.DownloadStream(t.Context(), store.container, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
}

func TestDigestVerifiedRead(t *testing.T) {
	store := testAzureStore(t, "goobers-digest")
	digest := digestFor([]byte("expected content"))
	key, err := blobKey(digest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.client.UploadBuffer(t.Context(), store.container, key, []byte("different content"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(t.Context(), digest); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("Get error = %v, want ErrDigestMismatch", err)
	}
}

func TestAzureBlobStoreConformance(t *testing.T) {
	store := testAzureStore(t, "goobers-conformance")
	data := []byte("content-addressed payload")
	digest := digestFor(data)
	if err := store.Put(t.Context(), digest, data); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(t.Context(), digest, data); err != nil {
		t.Fatalf("idempotent Put: %v", err)
	}
	if has, err := store.Has(t.Context(), digest); err != nil || !has {
		t.Fatalf("Has = %v, %v", has, err)
	}
	got, err := store.Get(t.Context(), digest)
	if err != nil || string(got) != string(data) {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if err := store.Remove(t.Context(), digest); err != nil {
		t.Fatal(err)
	}
	if has, err := store.Has(t.Context(), digest); err != nil || has {
		t.Fatalf("Has after Remove = %v, %v", has, err)
	}
}

func TestIdempotentPutAdvancesETag(t *testing.T) {
	store := testAzureStore(t, "goobers-put-touch")
	data := []byte("shared writer payload")
	digest := digestFor(data)
	if err := store.Put(t.Context(), digest, data); err != nil {
		t.Fatal(err)
	}
	key, err := blobKey(digest)
	if err != nil {
		t.Fatal(err)
	}
	client := store.client.ServiceClient().NewContainerClient(
		store.container).NewBlobClient(key)
	first, err := client.GetProperties(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(t.Context(), digest, data); err != nil {
		t.Fatal(err)
	}
	second, err := client.GetProperties(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.ETag == nil || second.ETag == nil || *first.ETag == *second.ETag {
		t.Fatalf("idempotent Put ETags = %v / %v, want change",
			first.ETag, second.ETag)
	}
}

func TestAzureBoundedRead(t *testing.T) {
	store := testAzureStore(t, "goobers-bounded-read")
	data := []byte("bounded generation archive")
	digest := digestFor(data)
	if err := store.Put(t.Context(), digest, data); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetBounded(t.Context(), digest, int64(len(data)))
	if err != nil || string(got) != string(data) {
		t.Fatalf("GetBounded = %q, %v", got, err)
	}
	if _, err := store.GetBounded(
		t.Context(), digest, int64(len(data)-1),
	); !errors.Is(err, blobstore.ErrTooLarge) {
		t.Fatalf("undersized GetBounded = %v, want ErrTooLarge", err)
	}
}

func testAzureStore(t *testing.T, container string) *Azure {
	t.Helper()
	account := os.Getenv("GOOBERS_TEST_AZURITE_ACCOUNT")
	key := os.Getenv("GOOBERS_TEST_AZURITE_KEY")
	serviceURL := os.Getenv("GOOBERS_TEST_AZURITE_URL")
	if account == "" || key == "" || serviceURL == "" {
		t.Skip("Azurite test environment is not configured")
	}
	credential, err := azblob.NewSharedKeyCredential(account, key)
	if err != nil {
		t.Fatal(err)
	}
	client, err := azblob.NewClientWithSharedKeyCredential(serviceURL, credential, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := newAzure(client, container)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.client.CreateContainer(t.Context(), container, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.client.DeleteContainer(context.Background(), container, nil)
	})
	return store
}
