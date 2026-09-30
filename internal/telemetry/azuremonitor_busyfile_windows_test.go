package telemetry

import (
	"testing"

	"golang.org/x/sys/windows"
)

func lockReplayFileForTest(t testing.TB, path string) func() {
	t.Helper()
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(encoded, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("lock replay file: %v", err)
	}
	return func() {
		if err := windows.CloseHandle(handle); err != nil {
			t.Fatalf("unlock replay file: %v", err)
		}
	}
}
