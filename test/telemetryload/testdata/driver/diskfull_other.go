//go:build !linux

package main

import "errors"

func smallTmpfs(string) error {
	return errors.New("disk-full fixture requires an isolated Linux tmpfs; other platforms need separate native-volume validation")
}
