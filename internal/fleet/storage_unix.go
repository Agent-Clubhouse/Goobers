//go:build !windows

package fleet

import "os"

func platformStorageBaseDir() (string, error) {
	return os.UserConfigDir()
}

// protect is intentionally a byte copy on Unix. Secret confidentiality comes
// from the association directory's 0700 mode and each secret file's 0600 mode.
func protect(plaintext []byte) ([]byte, error) {
	return append([]byte(nil), plaintext...), nil
}

func unprotect(ciphertext []byte) ([]byte, error) {
	return append([]byte(nil), ciphertext...), nil
}
