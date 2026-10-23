package objstore

import (
	"strings"
	"testing"
)

func TestBlobKeyLayout(t *testing.T) {
	digest := "sha256:" + strings.Repeat("ab", 32)
	key, err := blobKey(digest)
	if err != nil {
		t.Fatal(err)
	}
	if key != "blobs/sha256/ab/"+strings.Repeat("ab", 31) {
		t.Fatalf("key = %q", key)
	}
}
