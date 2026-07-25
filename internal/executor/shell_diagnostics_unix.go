//go:build !windows

package executor

import (
	"bytes"
	"os/exec"
	"runtime"
	"strconv"
)

func defaultDiagnosticsCapture(pid int) []byte {
	var b bytes.Buffer
	spid := strconv.Itoa(pid)
	if out, err := exec.Command("ps", "-eo", "pid,ppid,pgid,etime,stat,command").Output(); err == nil {
		b.WriteString("--- process tree (make / go test / .test / git / sandbox / goobers) ---\n")
		for _, line := range bytes.Split(out, []byte("\n")) {
			for _, kw := range []string{"make", "go test", ".test", "git ", "sandbox", "goobers", "PID"} {
				if bytes.Contains(line, []byte(kw)) {
					b.Write(line)
					b.WriteByte('\n')
					break
				}
			}
		}
	}
	if out, err := exec.Command("lsof", "-p", spid).Output(); err == nil {
		b.WriteString("\n--- lsof (open fds — PIPE/FIFO reveal I/O-deadlock partners) ---\n")
		for _, line := range bytes.Split(out, []byte("\n")) {
			if bytes.Contains(line, []byte("PIPE")) || bytes.Contains(line, []byte("FIFO")) ||
				bytes.Contains(line, []byte("REG")) || bytes.Contains(line, []byte("COMMAND")) {
				b.Write(line)
				b.WriteByte('\n')
			}
		}
	}
	if runtime.GOOS == "darwin" {
		// `sample` uses the OS thread sampler (no runtime cooperation), so it
		// captures native stacks of a stage wedged in a syscall that SIGQUIT
		// can't dump. It briefly SIGSTOPs+SIGCONTs the target — harmless for a
		// stage that is already hung, and the watchdog is bounded to finish
		// before the timeout path ever signals the process.
		if out, err := exec.Command("sample", spid, "3").Output(); err == nil {
			b.WriteString("\n--- sample (native thread stacks) ---\n")
			b.Write(out)
		}
	}
	return b.Bytes()
}
