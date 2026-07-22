//go:build !windows

package main

import (
	"errors"
	"syscall"
)

func dashboardAddressInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}
