//go:build !embed_portal

package main

import (
	"errors"
	"testing"

	"github.com/goobers/goobers/internal/portalassets"
)

func TestDashboardAssetFSReportsMissingEmbeddedArtifact(t *testing.T) {
	if _, err := dashboardAssetFS(""); !errors.Is(err, portalassets.ErrNotEmbedded) {
		t.Fatalf("dashboardAssetFS(\"\") error = %v, want ErrNotEmbedded", err)
	}
}
