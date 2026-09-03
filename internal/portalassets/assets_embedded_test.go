//go:build embed_portal

package portalassets

import (
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
	for _, name := range []string{"portal-artifact.json"} {
		if _, err := fs.Stat(assets, name); err != nil {
			t.Errorf("embedded artifact is missing %q: %v", name, err)
		}
	}
}
