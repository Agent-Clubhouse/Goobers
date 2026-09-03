//go:build !embed_portal

package portalassets

import (
	"errors"
	"testing"
)

func TestFSReportsMissingEmbeddedAssets(t *testing.T) {
	if _, err := FS(); !errors.Is(err, ErrNotEmbedded) {
		t.Fatalf("FS() error = %v, want ErrNotEmbedded", err)
	}
}
