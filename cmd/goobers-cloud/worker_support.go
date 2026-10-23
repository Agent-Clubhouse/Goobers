package main

import (
	"github.com/goobers/goobers/internal/instance"
)

var loadConfigDirectory = instance.LoadConfigDir
var diagnosticsMode bool

const diagnosticsMaxOutputBytes int64 = 64 << 20
