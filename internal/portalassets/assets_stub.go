//go:build !embed_portal

package portalassets

import "io/fs"

func embeddedFS() (fs.FS, error) {
	return nil, ErrNotEmbedded
}
