package childresult

import (
	"testing"

	"github.com/goobers/goobers/internal/blobstore"
)

// The canary is fixed per instance and derivable on both sides.
func TestBlobPlaneCanaryIsFixedPerInstance(t *testing.T) {
	digest, data := BlobPlaneCanary("instance-a")
	again, _ := BlobPlaneCanary("instance-a")
	other, _ := BlobPlaneCanary("instance-b")
	if !blobstore.ValidDigest(digest) || digest != again || digest == other || len(data) == 0 {
		t.Fatalf("canary digests %s / %s / %s", digest, again, other)
	}
}
