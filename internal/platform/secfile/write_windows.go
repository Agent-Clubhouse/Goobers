//go:build windows

package secfile

import (
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// privateDescriptor returns a security descriptor whose protected DACL grants
// full access to the current user, SYSTEM and Administrators only. "P" marks
// the DACL protected, so nothing is inherited from the parent directory.
func privateDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")(A;;FA;;;SY)(A;;FA;;;BA)")
}

// writePrivate reports opened=true once path has been opened (and so may
// hold partial content), letting WritePrivate clean up on a later failure.
func writePrivate(path string, data []byte) (opened bool, err error) {
	sd, err := privateDescriptor()
	if err != nil {
		return false, err
	}
	defer runtime.KeepAlive(sd)
	acl, _, err := sd.DACL()
	if err != nil {
		return false, err
	}
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	// A new file receives the private DACL atomically at creation. CreateFile
	// ignores the descriptor when the file already exists, so the DACL is also
	// set explicitly on the handle below, before truncating or writing.
	// Share mode 0 keeps anything else from opening the file while it is being
	// replaced; it means the write fails (rather than racing) if another
	// process already holds the file open, which callers writing a fresh
	// per-run destination never hit.
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	handle, err := windows.CreateFile(encoded,
		windows.GENERIC_WRITE|windows.WRITE_DAC|windows.READ_CONTROL,
		0, sa, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return false, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(handle), path)
	defer func() {
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}()
	if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		return true, &os.PathError{Op: "set DACL", Path: path, Err: err}
	}
	if err := f.Truncate(0); err != nil {
		return true, err
	}
	_, err = f.Write(data)
	return true, err
}
