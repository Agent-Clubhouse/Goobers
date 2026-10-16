//go:build windows

package secfile

import (
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

func createPrivateTemp(dir string) (*os.File, error) {
	sd, err := privateDescriptor()
	if err != nil {
		return nil, err
	}
	defer runtime.KeepAlive(sd)
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	for range 10 {
		path := filepath.Join(dir, ".private-"+rand.Text())
		encoded, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return nil, err
		}
		handle, err := windows.CreateFile(encoded, windows.GENERIC_WRITE|windows.READ_CONTROL,
			0, sa, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if errors.Is(err, windows.ERROR_FILE_EXISTS) || errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			continue
		}
		if err != nil {
			return nil, &os.PathError{Op: "create private temporary file", Path: path, Err: err}
		}
		return os.NewFile(uintptr(handle), path), nil
	}
	return nil, &os.PathError{Op: "create private temporary file", Path: dir, Err: os.ErrExist}
}
