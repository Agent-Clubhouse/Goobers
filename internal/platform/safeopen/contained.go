package safeopen

import (
	"errors"
	"os"
)

// ErrNotRegular reports a file type that cannot be consumed as bounded data.
var ErrNotRegular = errors.New("safeopen: expected a regular file")

// OpenRegularInRoot opens a regular file inside an os.Root. Unlike Open, safe
// internal symlinks are allowed; os.Root enforces containment during traversal.
// Unix opens are nonblocking so a FIFO swapped in before open cannot hang a
// reader. Both platforms validate the opened handle, not merely a prior stat.
func OpenRegularInRoot(root *os.Root, name string) (*os.File, error) {
	info, err := root.Stat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, ErrNotRegular
	}
	file, err := openInRoot(root, name)
	if err != nil {
		return nil, err
	}
	info, err = file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = ErrNotRegular
	}
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}
