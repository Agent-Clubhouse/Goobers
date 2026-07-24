//go:build windows

package executor

import (
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
)

func defaultDiagnosticsCapture(pid int) []byte {
	var b bytes.Buffer
	spid := strconv.Itoa(pid)
	if out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %s", spid), "/V").Output(); err == nil {
		b.WriteString("--- process info (tasklist) ---\n")
		b.Write(out)
	}
	if out, err := exec.Command("handle.exe", "-p", spid, "-nobanner").Output(); err == nil {
		b.WriteString("\n--- open handles (handle.exe) ---\n")
		b.Write(out)
	}
	return b.Bytes()
}
