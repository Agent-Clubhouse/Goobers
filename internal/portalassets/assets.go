package portalassets

import (
	"errors"
	"io/fs"
)

// ErrNotEmbedded reports that the binary was built without the Portal artifact.
var ErrNotEmbedded = errors.New("portal assets are not embedded; build a complete binary with `make build-goobers` or pass --dev-assets")

// FS returns the embedded production Portal asset filesystem.
func FS() (fs.FS, error) {
	return embeddedFS()
}
