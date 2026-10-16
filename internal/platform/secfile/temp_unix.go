//go:build unix

package secfile

import "os"

func createPrivateTemp(dir string) (*os.File, error) {
	return os.CreateTemp(dir, ".private-*")
}
