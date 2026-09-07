package oidcauth

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// vitePrefix is the build-time variable prefix the portal's deleted OIDC client
// used. docs/guides/oidc-authentication.md documented these for months after
// #3122 removed portal/src/auth, so an operator could configure a sign-in that
// the bundle silently ignores (#4519).
const vitePrefix = "VITE_OIDC"

func TestOIDCGuideDocumentsPortalVariablesOnlyIfPortalReadsThem(t *testing.T) {
	t.Parallel()

	repoRoot := filepath.Join("..", "..")

	portalReads := false
	portalSrc := filepath.Join(repoRoot, "portal", "src")
	err := filepath.WalkDir(portalSrc, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		switch filepath.Ext(path) {
		case ".ts", ".tsx", ".js", ".jsx":
		default:
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(raw), vitePrefix) {
			portalReads = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk portal sources: %v", err)
	}

	guidePath := filepath.Join(repoRoot, "docs", "guides", "oidc-authentication.md")
	guideRaw, err := os.ReadFile(guidePath)
	if err != nil {
		t.Fatalf("read OIDC guide: %v", err)
	}
	// Match the copyable shape (an assignment in a build command), not a bare
	// mention: the guide legitimately names these variables while telling
	// operators NOT to set them.
	guideConfigures := strings.Contains(string(guideRaw), vitePrefix+"_ISSUER=") &&
		strings.Contains(string(guideRaw), vitePrefix+"_CLIENT_ID=")

	if guideConfigures && !portalReads {
		t.Errorf("oidc-authentication.md tells operators to set %s_* build variables, but no file under portal/src reads %s; the setup is silently ignored", vitePrefix, vitePrefix)
	}
	if portalReads && !guideConfigures {
		t.Errorf("portal/src reads %s build variables, but oidc-authentication.md no longer documents how to set them", vitePrefix)
	}
}
