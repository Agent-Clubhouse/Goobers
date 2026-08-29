//go:build windows

package executor

import (
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
)

// stageCmd is unused here: tasklist/handle.exe are scoped to pid already, so
// there is no keyword filter to broaden (that gap is unix-only, #2172).
func defaultDiagnosticsCapture(pid int, _ string) []byte {
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
