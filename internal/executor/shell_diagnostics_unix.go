//go:build !windows

package executor

import (
	"bytes"
	"os/exec"
	"runtime"
	"strconv"
)

// diagnosticsKeywords returns the ps-output substrings that mark a line as
// worth keeping in the captured process tree: the stage-neutral keywords
// (git/sandbox/goobers/the PID header) plus the Go-specific ones, plus —
// when stageCmd is non-empty — the hung stage's own command basename, so a
// wedged npm/dotnet/pytest/mvn process is captured too, not just Go ones
// (#2172).
func diagnosticsKeywords(stageCmd string) []string {
	keywords := []string{"make", "go test", ".test", "git ", "sandbox", "goobers", "PID"}
	if stageCmd != "" {
		keywords = append(keywords, stageCmd)
	}
	return keywords
}

// filterProcessTreeLines keeps only the ps-output lines that contain at least
// one of keywords — extracted from defaultDiagnosticsCapture so the filter
// itself is testable against synthetic ps fixtures without shelling out
// (#2172).
func filterProcessTreeLines(psOutput []byte, keywords []string) []byte {
	var b bytes.Buffer
	for _, line := range bytes.Split(psOutput, []byte("\n")) {
		for _, kw := range keywords {
			if bytes.Contains(line, []byte(kw)) {
				b.Write(line)
				b.WriteByte('\n')
				break
			}
		}
	}
	return b.Bytes()
}

func defaultDiagnosticsCapture(pid int, stageCmd string) []byte {
	var b bytes.Buffer
	spid := strconv.Itoa(pid)
	keywords := diagnosticsKeywords(stageCmd)
	if out, err := exec.Command("ps", "-eo", "pid,ppid,pgid,etime,stat,command").Output(); err == nil {
		b.WriteString("--- process tree (stage command / make / go test / .test / git / sandbox / goobers) ---\n")
		b.Write(filterProcessTreeLines(out, keywords))
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
