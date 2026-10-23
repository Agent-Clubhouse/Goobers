package main

import (
	"github.com/goobers/goobers/internal/runtimewiring"
)

type repeatableFlag = runtimewiring.RepeatableFlag

func onOff(enabled bool) string {
	if enabled {
		return "on"
	}
	return "off"
}
