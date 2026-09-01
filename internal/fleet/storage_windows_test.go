//go:build windows

package fleet

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestPlatformStorageBaseDirUsesLocalAppDataKnownFolder(t *testing.T) {
	want := filepath.Join(t.TempDir(), "Local")
	original := knownFolderPath
	t.Cleanup(func() { knownFolderPath = original })

	var requested windows.KNOWNFOLDERID
	var flags uint32
	knownFolderPath = func(folderID *windows.KNOWNFOLDERID, requestedFlags uint32) (string, error) {
		requested = *folderID
		flags = requestedFlags
		return want, nil
	}
	t.Setenv("APPDATA", filepath.Join(t.TempDir(), "OneDrive", "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(t.TempDir(), "wrong"))

	got, err := platformStorageBaseDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("platformStorageBaseDir = %q, want %q", got, want)
	}
	if requested != *windows.FOLDERID_LocalAppData {
		t.Fatalf("requested known folder = %v, want LocalAppData", requested)
	}
	if flags != windows.KF_FLAG_DEFAULT {
		t.Fatalf("known-folder flags = %d, want %d", flags, windows.KF_FLAG_DEFAULT)
	}
}
