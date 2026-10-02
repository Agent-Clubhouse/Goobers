//go:build unix

package secfile

import "os"

// writePrivate reports opened=true once path has been opened (and so may
// hold partial content), letting WritePrivate clean up on a later failure.
func writePrivate(path string, data []byte) (opened bool, err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return false, err
	}
	defer func() {
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}()
	// The create mode is ignored for a file that already exists, so narrow it
	// explicitly before the secret is written.
	if err := f.Chmod(0o600); err != nil {
		return true, err
	}
	_, err = f.Write(data)
	return true, err
}
