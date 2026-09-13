//go:build windows

package configmirror

import (
	"errors"
	"os"
	"time"

	"github.com/goobers/goobers/internal/platform/safeopen"
)

// openRetryWindow bounds how long openArchive waits out the transient window
// in which a name that replaceSnapshot (MoveFileEx or FileRenameInfoEx) just
// published is not yet resolvable by CreateFile. #4670: on Windows CI,
// opening SnapshotName immediately after PublishValidated reports the
// atomic replace complete intermittently fails with ERROR_FILE_NOT_FOUND for
// a few milliseconds. Mirrors the retry window durability.ReplaceFile and
// durability.RemoveFile already use for the same class of transient Windows
// filesystem contention (durability_windows.go), extending that contract to
// the read side: a successful PublishValidated must let the very next Open
// observe it.
const openRetryWindow = 2 * time.Second

func openArchive(path string) (*os.File, error) {
	deadline := time.Now().Add(openRetryWindow)
	for {
		f, err := safeopen.Open(path)
		if err == nil || !errors.Is(err, os.ErrNotExist) || time.Now().After(deadline) {
			return f, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}
