//go:build linux

package proc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestAliveRechecksZombieReapedDuringProbe(t *testing.T) {
	pid := startZombieChild(t, false)
	probes := 0
	got := probeProcessAlive(func() error {
		probes++
		err := syscall.Kill(pid, 0)
		if probes == 1 && err != nil {
			t.Fatalf("initial zombie probe: %v", err)
		}
		return err
	}, func() bool {
		var status syscall.WaitStatus
		if _, err := syscall.Wait4(pid, &status, 0, nil); err != nil {
			t.Fatal(err)
		}
		return zombie(pid)
	})
	if got {
		t.Fatal("reaped child reported alive after zombie observation")
	}
}

func TestQuiescenceRechecksIdentityReapedDuringProbe(t *testing.T) {
	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	started, ok := StartTime(cmd.Process.Pid)
	if !ok {
		t.Fatal("live child identity unavailable")
	}
	present, err := processIdentityPresent(processIdentity{pid: cmd.Process.Pid, startTime: started}, Alive, func(pid int) (time.Time, bool) {
		if err := cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		_ = cmd.Wait()
		return StartTime(pid)
	})
	if present || err != nil {
		t.Fatalf("reaped child: present=%v err=%v", present, err)
	}
}

func TestQuiescenceUnknownLiveIdentityFailsClosed(t *testing.T) {
	_, err := processIdentityPresent(processIdentity{pid: os.Getpid()}, Alive, func(int) (time.Time, bool) { return time.Time{}, false })
	if !errors.Is(err, ErrQuiescenceUnobservable) {
		t.Fatalf("live unknown identity: %v", err)
	}
}

func TestAliveUnknownObservationFailsTowardAlive(t *testing.T) {
	if !probeProcessAlive(func() error { return syscall.EPERM }, func() bool { return false }) {
		t.Fatal("permission denial reported dead")
	}
	if !probeProcessAlive(func() error { return nil }, func() bool { return false }) {
		t.Fatal("unknown zombie state reported dead")
	}
}
