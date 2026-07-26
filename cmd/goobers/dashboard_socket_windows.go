//go:build windows

package main

import (
	"errors"

	"golang.org/x/sys/windows"
)

func dashboardAddressInUse(err error) bool {
	return errors.Is(err, windows.WSAEADDRINUSE)
}
