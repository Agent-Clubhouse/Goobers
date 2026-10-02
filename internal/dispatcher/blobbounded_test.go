package dispatcher

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/blobstore"
)

func TestBlobGetBoundedAndRefreshingAuthentication(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer worker-blob-only" {
			t.Error("missing worker bearer")
		}
		_, _ = w.Write([]byte("12345"))
	}))
	defer server.Close()
	client := &BlobClient{BaseURL: server.URL, Token: "wrong-static", TokenSource: func() (string, error) { calls++; return "worker-blob-only", nil }}
	if got, err := client.GetBounded(context.Background(), "digest", 4); got != nil || !errors.Is(err, blobstore.ErrTooLarge) {
		t.Fatalf("oversized response: %q %v", got, err)
	}
	if got, err := client.GetBounded(context.Background(), "digest", 5); string(got) != "12345" || err != nil {
		t.Fatalf("bounded response: %q %v", got, err)
	}
	if calls != 2 {
		t.Fatalf("minted %d credentials for two requests", calls)
	}
	client.TokenSource = func() (string, error) { return "", errors.New("mint unavailable") }
	if _, err := client.Get(context.Background(), "digest"); err == nil || !strings.Contains(err.Error(), "authentication") {
		t.Fatal("mint failure used static fallback")
	}
}
