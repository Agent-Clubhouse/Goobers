package main

import (
	"encoding/json"
	"io"
	"os"
)

type stageResultOptions struct {
	MarshalLabel       string
	WriteLabel         string
	Indented           bool
	TrailingNewline    bool
	WriteErrorExitCode int
}

func writeStageResultJSON[T any](stderr io.Writer, path string, value T, opts stageResultOptions) int {
	var (
		data []byte
		err  error
	)
	if opts.Indented {
		data, err = json.MarshalIndent(value, "", "  ")
	} else {
		data, err = json.Marshal(value)
	}
	if err != nil {
		pf(stderr, "error: %s: %v\n", opts.MarshalLabel, err)
		return 1
	}
	if opts.TrailingNewline {
		data = append(data, '\n')
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		writeLabel := opts.WriteLabel
		if writeLabel == "" {
			writeLabel = "write"
		}
		pf(stderr, "error: %s %s: %v\n", writeLabel, path, err)
		if opts.WriteErrorExitCode != 0 {
			return opts.WriteErrorExitCode
		}
		return 1
	}
	return 0
}
