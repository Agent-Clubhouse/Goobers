//go:build darwin

package sandbox

import (
	"context"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// Only signal processes this test started. A regression must never put the
// user's other processes at risk, even when confinement is entirely absent.
func TestNativeSandboxScopesSignals(t *testing.T) {
	s := requiredNativeSandbox(t)
	host := exec.Command("/bin/sleep", "60")
	if err := host.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = host.Process.Kill()
		_ = host.Wait()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/sh", "-c", `
if kill -TERM "$1"; then
    echo "sandbox allowed a signal to the host process"
    exit 91
fi
/bin/sleep 5 &
child=$!
kill -TERM "$child" || exit 92
wait "$child"
status=$?
test "$status" -gt 128
`, "signal-probe", strconv.Itoa(host.Process.Pid))
	if err := s.Wrap(command, Policy{Workspace: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("signal confinement: %v\n%s", err, output)
	}
	if err := host.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("host process did not survive: %v", err)
	}
}
