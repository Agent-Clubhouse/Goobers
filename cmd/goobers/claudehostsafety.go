package main

import (
	"io"
	"os"

	"github.com/goobers/goobers/internal/hostprocess"
)

func runClaudeHostSafety(args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		pln(stderr, "goobers: __claude-host-safety accepts only hook JSON on stdin")
		return 2
	}
	return hostprocess.ClaudeHook(os.Stdin, stdout, stderr)
}
