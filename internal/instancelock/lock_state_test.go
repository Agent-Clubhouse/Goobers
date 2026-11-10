package instancelock

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/platform/lock"
)

func TestInspectDaemonLockHeldState(t *testing.T) {
	for _, tc := range []struct {
		name    string
		data    string
		running bool
		wantErr string
	}{
		{name: "empty", running: true},
		{name: "whitespace", data: " \n\t", running: true},
		{name: "partial JSON", data: `{"holderKind":"daemon"`, running: true, wantErr: "decode lock state"},
		{name: "corrupt JSON", data: "not JSON", running: true, wantErr: "decode lock state"},
		{name: "missing identity", data: `{"holderKind":"daemon","holderPid":123}`, running: true, wantErr: "daemon holder does not match identity"},
		{name: "incomplete identity", data: `{"pid":123,"holderKind":"daemon","holderPid":123}`, running: true, wantErr: "decode daemon identity"},
		{name: "unknown holder", data: `{"holderKind":"unknown","holderPid":123}`, running: true, wantErr: "unknown holder kind"},
		{name: "manual", data: `{"holderKind":"manual","holderPid":123}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "up.lock")
			if err := os.WriteFile(path, []byte(tc.data), 0o600); err != nil {
				t.Fatal(err)
			}
			held, err := lock.TryAcquire(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = held.Release() }()
			running, identity, err := InspectDaemonLock(path)
			if running != tc.running || identity != nil {
				t.Fatalf("InspectDaemonLock = (%t, %+v, %v), want (%t, nil, error containing %q)", running, identity, err, tc.running, tc.wantErr)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestInspectDaemonLockManualHolderPreservesStoppedIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "up.lock")
	identity := lockStateTestIdentity(t)
	release, err := AcquireWithIdentity(path, &identity)
	if err != nil {
		t.Fatal(err)
	}
	release()
	manualRelease, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer manualRelease()
	running, got, err := InspectDaemonLock(path)
	if err != nil || running || got == nil || *got != identity {
		t.Fatalf("InspectDaemonLock = (%t, %+v, %v), want stopped with retained identity %+v", running, got, err, identity)
	}
}

func TestInspectDaemonLockUnheldState(t *testing.T) {
	identity := lockStateTestIdentity(t)
	data, err := json.Marshal(newInstanceLockState(&identity, HolderDaemon, identity.PID))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name         string
		data         string
		wantIdentity bool
		wantErr      bool
	}{
		{name: "empty"},
		{name: "corrupt", data: "not JSON", wantErr: true},
		{name: "stopped daemon", data: string(data), wantIdentity: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "up.lock")
			if err := os.WriteFile(path, []byte(tc.data), 0o600); err != nil {
				t.Fatal(err)
			}
			running, got, err := InspectDaemonLock(path)
			if running || (got != nil) != tc.wantIdentity || (err != nil) != tc.wantErr {
				t.Fatalf("InspectDaemonLock = (%t, %+v, %v), want stopped, identity=%t, error=%t", running, got, err, tc.wantIdentity, tc.wantErr)
			}
			if got != nil && *got != identity {
				t.Fatalf("identity = %+v, want %+v", got, identity)
			}
		})
	}
}

func TestWriteInstanceLockStateReplacesLongerRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "up.lock")
	held, err := lock.TryAcquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()
	identity := lockStateTestIdentity(t)
	identity.Version = strings.Repeat("v", 4096)
	if err := writeInstanceLockState(held.File(), newInstanceLockState(&identity, HolderDaemon, identity.PID)); err != nil {
		t.Fatal(err)
	}
	want := newInstanceLockState(nil, HolderManual, os.Getpid())
	if err := writeInstanceLockState(held.File(), want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadStatePath(path)
	if err != nil || got == nil || *got != want {
		t.Fatalf("ReadStatePath = (%+v, %v), want %+v without the old record's tail", got, err, want)
	}
	other, err := lock.TryAcquire(path)
	if other != nil {
		_ = other.Release()
	}
	if !errors.Is(err, lock.ErrHeld) {
		t.Fatalf("rewriting the state lost the lock: %v", err)
	}
}

func TestWriteInstanceLockStateConcurrentInspection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "up.lock")
	held, err := lock.TryAcquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()
	identity := lockStateTestIdentity(t)
	if err := writeInstanceLockState(held.File(), newInstanceLockState(&identity, HolderDaemon, identity.PID)); err != nil {
		t.Fatal(err)
	}

	// Readers have independent handles, as status/stop commands do. Alternating
	// record lengths exercises both growth and the old tail before truncation.
	start := make(chan struct{})
	var workers sync.WaitGroup
	workers.Go(func() {
		<-start
		for i := range 128 {
			identity.Version = "v1" + strings.Repeat("x", (i%2)*4096)
			if err := writeInstanceLockState(held.File(), newInstanceLockState(&identity, HolderDaemon, identity.PID)); err != nil {
				t.Errorf("write lock state: %v", err)
				return
			}
		}
	})
	for range 3 {
		workers.Go(func() {
			<-start
			for range 256 {
				state, err := ReadStatePath(path)
				if state == nil && err == nil {
					t.Error("reader observed an empty state during rewrite")
					return
				}
				// A shorter record can briefly have the previous record's tail.
				// Such a decode error must not turn a held lock into 'stopped'.
				if err != nil {
					var syntaxErr *json.SyntaxError
					if !errors.As(err, &syntaxErr) {
						t.Errorf("unexpected read error: %v", err)
						return
					}
				}
				running, got, err := InspectDaemonLock(path)
				if !running || (err == nil && got == nil) {
					t.Errorf("InspectDaemonLock during rewrite = (%t, %+v, %v)", running, got, err)
					return
				}
			}
		})
	}
	close(start)
	workers.Wait()
}

func lockStateTestIdentity(t *testing.T) DaemonIdentity {
	t.Helper()
	return DaemonIdentity{
		PID:          os.Getpid(),
		StartedAt:    time.Date(2026, time.October, 17, 0, 0, 0, 0, time.UTC),
		InstanceRoot: t.TempDir(),
		Version:      "v1",
	}
}
