//go:build windows

package instance

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// symlinkFlagRelative marks a SymbolicLinkReparseBuffer whose substitute name
// is relative to the link's own directory. golang.org/x/sys/windows does not
// export it at the version pinned here.
const symlinkFlagRelative = 0x00000001

// readReparsePoint returns the raw reparse buffer attached to path. ok is false
// when path carries no reparse point at all, which is not an error.
func readReparsePoint(path string) (data []byte, ok bool, err error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, false, err
	}
	attributes, err := windows.GetFileAttributes(name)
	if err != nil {
		return nil, false, err
	}
	if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		return nil, false, nil
	}
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return nil, false, err
	}
	buffer := make([]byte, windows.MAXIMUM_REPARSE_DATA_BUFFER_SIZE)
	var bytesReturned uint32
	reparseErr := windows.DeviceIoControl(
		handle,
		windows.FSCTL_GET_REPARSE_POINT,
		nil,
		0,
		&buffer[0],
		uint32(len(buffer)),
		&bytesReturned,
		nil,
	)
	closeErr := windows.CloseHandle(handle)
	if err := errors.Join(reparseErr, closeErr); err != nil {
		return nil, false, err
	}
	if bytesReturned < 4 {
		return nil, false, fmt.Errorf("read reparse point %s: response is %d bytes", path, bytesReturned)
	}
	return buffer[:bytesReturned], true, nil
}

func isLegacyRuntimeAlias(path string, info fs.FileInfo) (bool, error) {
	if info.Mode()&fs.ModeSymlink != 0 {
		return true, nil
	}
	data, ok, err := readReparsePoint(path)
	if err != nil || !ok {
		return false, err
	}
	tag := binary.LittleEndian.Uint32(data[:4])
	return tag == windows.IO_REPARSE_TAG_MOUNT_POINT || tag == windows.IO_REPARSE_TAG_SYMLINK, nil
}

// ResolveRuntimeAlias reports the target a compatibility alias points at.
//
// filepath.EvalSymlinks cannot do this on Windows. Go 1.23 stopped reporting
// directory junctions (IO_REPARSE_TAG_MOUNT_POINT) as symlinks, so EvalSymlinks
// walks straight past one and hands back the junction's own path instead of its
// target. CreateLegacyRuntimeAlias creates exactly such a junction, so the
// target has to come out of the reparse buffer — the same reason
// isLegacyRuntimeAlias cannot trust fs.ModeSymlink either. Exported so other
// packages that scan a runtime tree containing such an alias (e.g.
// internal/telemetry/rollup, #3280) can dedupe against it without
// reimplementing platform-specific reparse-point resolution.
func ResolveRuntimeAlias(path string) (string, error) {
	data, ok, err := readReparsePoint(path)
	if err != nil {
		return "", err
	}
	if !ok {
		return filepath.EvalSymlinks(path)
	}
	if len(data) < 16 {
		return "", fmt.Errorf("read reparse point %s: response is %d bytes", path, len(data))
	}

	// MountPointReparseBuffer and SymbolicLinkReparseBuffer share a header of
	// SubstituteNameOffset/Length + PrintNameOffset/Length at bytes 8..16; the
	// symlink form then carries a Flags word before its path buffer.
	pathBuffer := 16
	relative := false
	switch binary.LittleEndian.Uint32(data[:4]) {
	case windows.IO_REPARSE_TAG_MOUNT_POINT:
	case windows.IO_REPARSE_TAG_SYMLINK:
		if len(data) < 20 {
			return "", fmt.Errorf("read reparse point %s: response is %d bytes", path, len(data))
		}
		relative = binary.LittleEndian.Uint32(data[16:20])&symlinkFlagRelative != 0
		pathBuffer = 20
	default:
		// Some other reparse tag (dedup, OneDrive, ...). Not ours to interpret.
		return filepath.EvalSymlinks(path)
	}

	start := pathBuffer + int(binary.LittleEndian.Uint16(data[8:10]))
	length := int(binary.LittleEndian.Uint16(data[10:12]))
	if length%2 != 0 || start < pathBuffer || start+length > len(data) {
		return "", fmt.Errorf("reparse point %s has an out-of-range substitute name", path)
	}
	encoded := make([]uint16, length/2)
	for i := range encoded {
		encoded[i] = binary.LittleEndian.Uint16(data[start+2*i:])
	}

	target := strings.TrimPrefix(string(utf16.Decode(encoded)), `\??\`)
	if relative {
		target = filepath.Join(filepath.Dir(path), target)
	}
	// The stored target can carry 8.3 short components (C:\Users\RUNNER~1\...)
	// while callers compare against an EvalSymlinks-normalised path, so
	// normalise here rather than leaving every caller to remember.
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		return resolved, nil
	}
	return filepath.Clean(target), nil
}

// CreateLegacyRuntimeAlias creates the compatibility alias at legacy pointing
// at scoped, as a directory junction (see ResolveRuntimeAlias for why plain
// symlinks aren't used here). Exported so other packages/tests can construct
// the same platform-native alias this package's own migration path creates.
func CreateLegacyRuntimeAlias(legacy, scoped string) error {
	target, err := filepath.Abs(scoped)
	if err != nil {
		return err
	}
	target = filepath.Clean(target)
	substituteName, err := windows.UTF16FromString(`\??\` + target)
	if err != nil {
		return err
	}
	printName, err := windows.UTF16FromString(target)
	if err != nil {
		return err
	}

	pathBytes := 2 * (len(substituteName) + len(printName))
	if pathBytes+8 > int(^uint16(0)) {
		return fmt.Errorf("junction target path is too long: %s", target)
	}
	data := make([]byte, 16+pathBytes)
	binary.LittleEndian.PutUint32(data[0:4], windows.IO_REPARSE_TAG_MOUNT_POINT)
	binary.LittleEndian.PutUint16(data[4:6], uint16(8+pathBytes))
	binary.LittleEndian.PutUint16(data[8:10], 0)
	binary.LittleEndian.PutUint16(data[10:12], uint16(2*(len(substituteName)-1)))
	binary.LittleEndian.PutUint16(data[12:14], uint16(2*len(substituteName)))
	binary.LittleEndian.PutUint16(data[14:16], uint16(2*(len(printName)-1)))
	for i, value := range append(substituteName, printName...) {
		binary.LittleEndian.PutUint16(data[16+2*i:], value)
	}

	if err := os.Mkdir(legacy, 0o755); err != nil {
		return err
	}
	removeAlias := true
	defer func() {
		if removeAlias {
			_ = os.Remove(legacy)
		}
	}()

	name, err := windows.UTF16PtrFromString(legacy)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_WRITE,
		0,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return err
	}
	var bytesReturned uint32
	setErr := windows.DeviceIoControl(
		handle,
		windows.FSCTL_SET_REPARSE_POINT,
		&data[0],
		uint32(len(data)),
		nil,
		0,
		&bytesReturned,
		nil,
	)
	closeErr := windows.CloseHandle(handle)
	if err := errors.Join(setErr, closeErr); err != nil {
		return err
	}
	removeAlias = false
	return nil
}
