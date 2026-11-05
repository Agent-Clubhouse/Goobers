package main

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func TestWorkerStageBinaryResolvesLocalCLI(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			name := "goobers"
			if goos == "windows" {
				name += ".exe"
			}
			cloud := filepath.Join(t.TempDir(), "goobers-cloud")
			sibling := filepath.Join(filepath.Dir(cloud), name)
			fromPath := filepath.Join(t.TempDir(), name)
			for _, source := range []string{"sibling", "path", "missing"} {
				t.Run(source, func(t *testing.T) {
					var calls []string
					binary, err := findWorkerExecutorBinary(cloud, goos, func(candidate string) (string, error) {
						calls = append(calls, candidate)
						if source == "sibling" && candidate == sibling {
							return sibling, nil
						}
						if source == "path" && candidate == name {
							return fromPath, nil
						}
						return "", errors.New("fixture executable absent")
					})
					if source == "missing" {
						if err == nil || binary != "" {
							t.Fatalf("missing stage CLI accepted: %q %v", binary, err)
						}
						return
					}
					want := sibling
					wantCalls := []string{sibling}
					if source == "path" {
						want = fromPath
						wantCalls = append(wantCalls, name)
					}
					if err != nil || binary != want || !reflect.DeepEqual(calls, wantCalls) {
						t.Fatalf("binary=%q calls=%v err=%v; want %q %v", binary, calls, err, want, wantCalls)
					}
					if binary == cloud {
						t.Fatal("stage command routed to the cloud role CLI")
					}
				})
			}
		})
	}
}
