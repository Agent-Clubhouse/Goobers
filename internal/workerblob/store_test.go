package workerblob

import (
	"context"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/blobstore"
)

func TestResolveModes(t *testing.T) {
	for _, tt := range []struct {
		dir, endpoint, env string
		instance, refuse   bool
		want               string
	}{
		{"", "", "", true, true, ""}, {"dir", "http://plane", "", true, true, ""},
		{"dir", "", "http://stage-plane", true, false, ""},
		{"", "", "http://plane", true, false, "http://plane"},
		{"", "http://explicit", "http://default", true, false, "http://explicit"},
		{"", "", "", false, false, ""},
		{"", "", "https://stage-only.invalid?unused", false, false, ""},
	} {
		got, err := Resolve(tt.dir, tt.endpoint, tt.env, tt.instance)
		if (err != nil) != tt.refuse || got != tt.want {
			t.Fatalf("resolve %+v = %q %v", tt, got, err)
		}
	}
	for _, raw := range []string{"https://user:secret@host", "https://host?secret=x", "https://host/#secret", "file:///secret", "http://host:99999"} {
		if err := ValidateEndpoint(raw); err == nil || strings.Contains(err.Error(), raw) {
			t.Fatal("unsafe endpoint accepted or echoed")
		}
	}
}

func TestDispatchDirectoryProbeRejectsDifferentStore(t *testing.T) {
	local, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	other, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyShared(context.Background(), local, other); err == nil || !strings.Contains(err.Error(), "WORKER_BLOB_STORE_MISMATCH") {
		t.Fatalf("different directory accepted: %v", err)
	}
	if err := VerifyShared(context.Background(), local, local); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), t.TempDir(), "", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), "", "http://plane", "", nil); err == nil {
		t.Fatal("unsigned endpoint accepted")
	}
}
