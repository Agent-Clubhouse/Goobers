//go:build windows

package fleet

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

var knownFolderPath = windows.KnownFolderPath

func platformStorageBaseDir() (string, error) {
	return knownFolderPath(windows.FOLDERID_LocalAppData, windows.KF_FLAG_DEFAULT)
}

func protect(plaintext []byte) ([]byte, error) {
	return cryptData(plaintext, true)
}

func unprotect(ciphertext []byte) ([]byte, error) {
	return cryptData(ciphertext, false)
}

func cryptData(input []byte, encrypt bool) ([]byte, error) {
	if len(input) == 0 {
		return nil, fmt.Errorf("input must not be empty")
	}
	in := windows.DataBlob{Size: uint32(len(input)), Data: &input[0]}
	var out windows.DataBlob
	var err error
	if encrypt {
		description, convertErr := windows.UTF16PtrFromString("Goobers Fleet instance secret")
		if convertErr != nil {
			return nil, convertErr
		}
		err = windows.CryptProtectData(&in, description, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	} else {
		err = windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	}
	runtime.KeepAlive(input)
	if err != nil {
		return nil, err
	}
	defer func() { _, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data))) }()
	result := append([]byte(nil), unsafe.Slice(out.Data, out.Size)...)
	return result, nil
}
