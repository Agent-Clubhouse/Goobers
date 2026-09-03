//go:build embed_portal

package portalassets

import (
	"embed"
	"io/fs"
)

//go:embed dist
var assets embed.FS

func embeddedFS() (fs.FS, error) {
	return fs.Sub(assets, "dist")
}
