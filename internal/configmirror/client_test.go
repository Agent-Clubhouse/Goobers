package configmirror

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
)

func TestClientFetchesConfigByDigest(t *testing.T) {
	const instanceID = "0123456789abcdef0123456789abcdef"
	root := t.TempDir()
	config := filepath.Join(root, "config")
	writeTestFile(t, filepath.Join(config, "manifest.yaml"), "generation: one")
	plane, err := NewPlane(filepath.Join(root, "plane"))
	if err != nil {
		t.Fatal(err)
	}
	digest, err := plane.PublishValidated(t.Context(), config, []byte("instance"), func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requested := request.URL.Query().Get("digest")
		if requested == "" {
			_, _ = fmt.Fprintf(w, `{"digest":%q,"instanceID":%q}`, plane.Current(), instanceID)
			return
		}
		file, size, err := plane.Open(requested)
		if err != nil {
			http.NotFound(w, request)
			return
		}
		defer func() { _ = file.Close() }()
		w.Header().Set(apicontract.DigestHeader, requested)
		w.Header().Set("Content-Length", fmt.Sprint(size))
		_, _ = io.Copy(w, file)
	}))
	t.Cleanup(server.Close)

	client := Client{BaseURL: server.URL, HTTP: server.Client()}
	current, err := client.Current(t.Context())
	if err != nil || current.Digest != digest || current.InstanceID != instanceID {
		t.Fatalf("Current = %q, %v", current, err)
	}
	directory, err := client.Materialize(t.Context(), filepath.Join(root, "cache"), current.Digest, func(path string) error {
		_, err := os.Stat(filepath.Join(path, "config", "manifest.yaml"))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(directory, "config", "manifest.yaml")); err != nil || string(data) != "generation: one" {
		t.Fatalf("materialized = %q, %v", data, err)
	}
}
