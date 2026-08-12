//go:build windows

package instance

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

const (
	fsctlSetReparsePoint      = 0x000900a4
	ioReparseTagMountPoint    = 0xa0000003
	mountPointReparseDataSize = 8
	reparseDataHeaderSize     = 8
	mountPointHeaderSize      = reparseDataHeaderSize + mountPointReparseDataSize
)

func createLegacyRuntimeAlias(legacy, scoped string) (err error) {
	target, err := filepath.Abs(scoped)
	if err != nil {
		return err
	}
	target = filepath.Clean(target)
	substituteName := junctionSubstituteName(target)
	substituteUTF16, err := windows.UTF16FromString(substituteName)
	if err != nil {
		return err
	}
	printUTF16, err := windows.UTF16FromString(target)
	if err != nil {
		return err
	}

	pathBytes := 2 * (len(substituteUTF16) + len(printUTF16))
	dataSize := mountPointReparseDataSize + pathBytes
	if reparseDataHeaderSize+dataSize > windows.MAXIMUM_REPARSE_DATA_BUFFER_SIZE {
		return fmt.Errorf("junction target is too long: %s", target)
	}
	reparseData := make([]byte, reparseDataHeaderSize+dataSize)
	binary.LittleEndian.PutUint32(reparseData[0:4], ioReparseTagMountPoint)
	binary.LittleEndian.PutUint16(reparseData[4:6], uint16(dataSize))
	binary.LittleEndian.PutUint16(reparseData[10:12], uint16(2*(len(substituteUTF16)-1)))
	binary.LittleEndian.PutUint16(reparseData[12:14], uint16(2*len(substituteUTF16)))
	binary.LittleEndian.PutUint16(reparseData[14:16], uint16(2*(len(printUTF16)-1)))
	writeUTF16(reparseData[mountPointHeaderSize:], substituteUTF16)
	writeUTF16(reparseData[mountPointHeaderSize+2*len(substituteUTF16):], printUTF16)

	if err := os.Mkdir(legacy, 0o755); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(legacy)
		}
	}()
	path, err := windows.UTF16PtrFromString(legacy)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(
		path,
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
	defer windows.CloseHandle(handle)

	var bytesReturned uint32
	return windows.DeviceIoControl(
		handle,
		fsctlSetReparsePoint,
		&reparseData[0],
		uint32(len(reparseData)),
		nil,
		0,
		&bytesReturned,
		nil,
	)
}

func junctionSubstituteName(target string) string {
	switch {
	case strings.HasPrefix(target, `\\?\UNC\`):
		return `\??\UNC\` + strings.TrimPrefix(target, `\\?\UNC\`)
	case strings.HasPrefix(target, `\\?\`):
		return `\??\` + strings.TrimPrefix(target, `\\?\`)
	case strings.HasPrefix(target, `\\`):
		return `\??\UNC\` + strings.TrimPrefix(target, `\\`)
	default:
		return `\??\` + target
	}
}

func writeUTF16(dst []byte, value []uint16) {
	for i, codeUnit := range value {
		binary.LittleEndian.PutUint16(dst[2*i:], codeUnit)
	}
}
