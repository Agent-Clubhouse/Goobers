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

// FromPath resolves path to an absolute file URI. It returns "" for an empty
// path, a path carrying query syntax (which a DSN suffix cannot be appended
// to), or a path that cannot be made absolute.
func FromPath(path string) string {
	if path == "" || strings.Contains(path, "?") {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	return File(abs)
}
