//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestSweepDelegateRequestsWaitsForWindowsFileSharing(t *testing.T) {
	for _, tt := range []struct {
		name  string
		share uint32
	}{
		{name: "read", share: 0},
		{name: "consume", share: windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := delegateFileTestProtocol()
			id, err := writeDelegateRequest(dir, cfg, delegateFileTestRequest{Value: "ready"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, cfg.pendingDir, id+cfg.requestSuffix)
			ptr, err := windows.UTF16PtrFromString(path)
			if err != nil {
				t.Fatal(err)
			}
			handle, err := windows.CreateFile(ptr, windows.GENERIC_READ, tt.share, nil, windows.OPEN_EXISTING, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			var once sync.Once
			release := func() { once.Do(func() { _ = windows.CloseHandle(handle) }) }
			t.Cleanup(release)
			if tt.name == "read" {
				if _, err := os.ReadFile(path); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
					t.Fatalf("read lock = %v, want sharing violation", err)
				}
			} else if err := os.Remove(path); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
				t.Fatalf("delete lock = %v, want sharing violation", err)
			}
			timer := time.AfterFunc(100*time.Millisecond, release)
			defer timer.Stop()
			dispatches := 0
			sweep := func() error {
				return sweepDelegateRequests(dir, cfg, time.Now,
					func(string, delegateFileTestRequest, error) (delegateFileTestResponse, bool) {
						return delegateFileTestResponse{}, true
					},
					func(req delegateFileTestRequest) delegateFileTestResponse {
						dispatches++
						if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("request still exists during dispatch: %v", err)
						}
						return delegateFileTestResponse{Value: req.Value}
					})
			}
			if err := sweep(); err != nil {
				t.Fatal(err)
			}
			if err := sweep(); err != nil {
				t.Fatal(err)
			}
			if dispatches != 1 {
				t.Fatalf("dispatches = %d, want exactly one", dispatches)
			}
			resp, ok := readAndRemoveDelegateJSON[delegateFileTestResponse](filepath.Join(dir, cfg.pendingDir, id+cfg.responseSuffix))
			if !ok || resp.Value != "ready" {
				t.Fatalf("response = %+v, read = %t", resp, ok)
			}
		})
	}
}
