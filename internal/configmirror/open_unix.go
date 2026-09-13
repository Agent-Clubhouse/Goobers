//go:build !windows

package configmirror

import (
	"os"

	"github.com/goobers/goobers/internal/platform/safeopen"
)

// openArchive opens the published snapshot. Unix renames are a single atomic
// syscall with no post-rename visibility delay, so no retry is needed here;
// see open_windows.go for the platform that does need one.
func openArchive(path string) (*os.File, error) {
	return safeopen.Open(path)
}
