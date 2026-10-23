package main

import (
	"strings"
)

type repeatableFlag []string

func (f *repeatableFlag) String() string { return strings.Join(*f, ",") }
func (f *repeatableFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func onOff(enabled bool) string {
	if enabled {
		return "on"
	}
	return "off"
}
