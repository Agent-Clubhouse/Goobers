//go:build unix

package safeopen

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// openFlags opens a component read-only and refuses to follow a symlink at it
// (O_NOFOLLOW → ELOOP), never leaks the descriptor across an exec (O_CLOEXEC),
// and never blocks on a FIFO/device that slipped in (O_NONBLOCK). This is the
// exact flag set the asset loader relied on before the seam existed, so unix
// behavior is byte-identical.
const openFlags = unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK

func openInRoot(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
}

func open(path string) (*os.File, error) {
	fd, err := unix.Open(path, openFlags, 0)
	if err != nil {
		return nil, noFollowError(err)
	}
	return os.NewFile(uintptr(fd), path), nil
}

func openAt(dir *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, openFlags, 0)
	if err != nil {
		return nil, noFollowError(err)
	}
	return os.NewFile(uintptr(fd), name), nil
}

func appendAt(dir *os.File, name string) (*os.File, error) {
	flags := unix.O_WRONLY | unix.O_APPEND | unix.O_CREAT | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	fd, err := unix.Openat(int(dir.Fd()), name, flags, 0o600)
	if err != nil {
		return nil, noFollowError(err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("safeopen: append requires a regular singly-linked file")
	}
	return os.NewFile(uintptr(fd), name), nil
}

// noFollowError maps the kernel's O_NOFOLLOW rejection (ELOOP) onto the
// ErrSymlink sentinel while keeping the raw errno in the chain, and passes
// every other error (e.g. ENOENT → fs.ErrNotExist) through untouched.
func noFollowError(err error) error {
	if errors.Is(err, unix.ELOOP) {
		return fmt.Errorf("%w: %w", ErrSymlink, err)
	}
	return err
}
