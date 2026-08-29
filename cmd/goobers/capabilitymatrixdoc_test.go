package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

// TestCapabilityMatrixDocUpToDate is the regen-diff guard (CONF-2, #2075):
// docs/provider-capability-matrix.md must match what renderCapabilityMatrix
// produces from live provider declarations, byte for byte, so the doc
// cannot drift from what providers actually declare. When a capability
// declaration changes intentionally, regenerate with
// UPDATE_GOLDEN=1 go test ./cmd/goobers -run TestCapabilityMatrixDocUpToDate
// (or `make docs`).
func TestCapabilityMatrixDocUpToDate(t *testing.T) {
	dir := docsDir(t)
	path := filepath.Join(dir, capabilityMatrixFile)

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := writeCapabilityMatrix(dir); err != nil {
			t.Fatalf("writeCapabilityMatrix: %v", err)
		}
		return
	}

	want := renderCapabilityMatrix()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", capabilityMatrixFile, err)
	}
	if string(got) != want {
		t.Fatalf("docs/%s is out of date; regenerate with make docs", capabilityMatrixFile)
	}
}

// TestCapabilityMatrixCoversEveryCapability guards against the generator's
// row loop silently dropping a capability the registry declares.
func TestCapabilityMatrixCoversEveryCapability(t *testing.T) {
	doc := renderCapabilityMatrix()
	caps := providers.AllCapabilities()
	if len(caps) == 0 {
		t.Fatal("registry returned no capabilities")
	}
	for _, cap := range caps {
		if !strings.Contains(doc, "`"+string(cap)+"`") {
			t.Errorf("capability %q missing from the generated matrix", cap)
		}
	}
}

func TestCapabilityMatrixDocumentsExperimentalGiteaPromotion(t *testing.T) {
	doc := renderCapabilityMatrix()
	for _, want := range []string{
		"| gitea | experimental |",
		"an in-repo `provider: gitea` config exercised in merge-tier CI and live conformance per #2441",
		"gitea (experimental)",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("generated capability matrix does not contain %q", want)
		}
	}
}
