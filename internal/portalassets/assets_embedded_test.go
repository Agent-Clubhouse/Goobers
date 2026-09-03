//go:build embed_portal

package portalassets

import (
	"encoding/json"
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

var indexAssetReference = regexp.MustCompile(`(?:src|href)="(/[^"]+)"`)

func TestEmbeddedFSContainsPortalEntryPoint(t *testing.T) {
	assets, err := FS()
	if err != nil {
		t.Fatal(err)
	}
	index, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		t.Fatalf("read embedded portal index: %v", err)
	}
	for _, match := range indexAssetReference.FindAllSubmatch(index, -1) {
		name := strings.TrimPrefix(string(match[1]), "/")
		if _, err := fs.Stat(assets, name); err != nil {
			t.Errorf("index references missing embedded asset %q: %v", name, err)
		}
	}
	manifestData, err := fs.ReadFile(assets, "portal-artifact.json")
	if err != nil {
		t.Fatalf("read embedded artifact manifest: %v", err)
	}
	var manifest struct {
		ArtifactVersion    int    `json:"artifactVersion"`
		PortalVersion      string `json:"portalVersion"`
		Commit             string `json:"commit"`
		APIContractVersion string `json:"apiContractVersion"`
		BasePath           string `json:"basePath"`
		APIBasePath        string `json:"apiBasePath"`
		GuidedBasePath     string `json:"guidedBasePath"`
	}
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatalf("decode embedded artifact manifest: %v", err)
	}
	if manifest.ArtifactVersion != 1 ||
		manifest.PortalVersion == "" ||
		manifest.Commit == "" ||
		manifest.APIContractVersion != "v1" ||
		manifest.BasePath != "/" ||
		manifest.APIBasePath != "/api" ||
		manifest.GuidedBasePath != "/guided" {
		t.Errorf("embedded artifact manifest = %+v", manifest)
	}
}
