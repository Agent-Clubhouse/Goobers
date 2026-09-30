package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

const serviceFatalStartupMarker = "fatal-startup:"

func appendServiceFatalStartup(layout instance.Layout, err error) error {
	file, openErr := os.OpenFile(layout.DaemonLogFile(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if openErr != nil {
		return openErr
	}
	defer func() { _ = file.Close() }()
	_, writeErr := fmt.Fprintf(file, "%s %s error: supervise daemon: %v\n", time.Now().UTC().Format(time.RFC3339Nano), serviceFatalStartupMarker, err)
	return writeErr
}

func latestServiceFatalStartup(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return ""
	}
	const maxTail = 64 << 10
	offset := max(info.Size()-maxTail, 0)
	buf := make([]byte, info.Size()-offset)
	if _, err := file.ReadAt(buf, offset); err != nil {
		return ""
	}
	lines := bytes.Split(buf, []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(string(lines[i]))
		if _, diagnostic, ok := strings.Cut(line, serviceFatalStartupMarker); ok {
			return strings.TrimSpace(diagnostic)
		}
	}
	return ""
}
