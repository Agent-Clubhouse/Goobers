// Package sqliteuri encodes already-validated absolute database paths without
// mistaking a Windows drive letter for a URI authority.
package sqliteuri

import (
	"net/url"
	"path/filepath"
	"strings"
)

// File renders an absolute native path as an escaped file URI. The caller owns
// path admission; this preserves the existing absolute-path storage contract.
func File(path string) string {
	slashed := filepath.ToSlash(path)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	return "file://" + (&url.URL{Path: slashed}).EscapedPath()
}
