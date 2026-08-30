//go:build !windows

package fleet

import "os"

func platformStorageBaseDir() (string, error) {
	return os.UserConfigDir()
}

func protect(plaintext []byte) ([]byte, error) {
	return append([]byte(nil), plaintext...), nil
}

func unprotect(ciphertext []byte) ([]byte, error) {
	return append([]byte(nil), ciphertext...), nil
}
