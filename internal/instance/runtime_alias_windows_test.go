//go:build windows

package instance

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/sys/windows"
)

const (
	fsctlGetReparsePoint   = 0x000900a8
	ioReparseTagMountPoint = 0xa0000003
)

func TestCreateLegacyRuntimeAliasCreatesJunction(t *testing.T) {
	root := filepath.Join(t.TempDir(), "instance & (100%) !^")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "gaggles", "alpha", "runs")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "run.yaml"), []byte("runId: test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "legacy & (runs) %!^")
	if err := createLegacyRuntimeAlias(alias, target); err != nil {
		t.Fatalf("createLegacyRuntimeAlias: %v", err)
	}

	if _, err := os.Stat(filepath.Join(alias, "run.yaml")); err != nil {
		t.Fatalf("read through junction: %v", err)
	}
	path, err := windows.UTF16PtrFromString(alias)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(
		path,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	data := make([]byte, windows.MAXIMUM_REPARSE_DATA_BUFFER_SIZE)
	var bytesReturned uint32
	if err := windows.DeviceIoControl(handle, fsctlGetReparsePoint, nil, 0, &data[0], uint32(len(data)), &bytesReturned, nil); err != nil {
		t.Fatal(err)
	}
	if tag := binary.LittleEndian.Uint32(data[:4]); tag != ioReparseTagMountPoint {
		t.Fatalf("reparse tag = %#x, want mount point %#x", tag, ioReparseTagMountPoint)
	}
}

func TestRuntimeDirsSkipJunctionAliases(t *testing.T) {
	layout := NewLayout(t.TempDir())
	if err := layout.EnsureGaggleRuntime("alpha"); err != nil {
		t.Fatal(err)
	}
	scoped := layout.ForGaggle("alpha")
	for _, pair := range [][2]string{
		{layout.RunsDir(), scoped.RunsDir()},
		{layout.WorkcopiesDir(), scoped.WorkcopiesDir()},
	} {
		if err := createLegacyRuntimeAlias(pair[0], pair[1]); err != nil {
			t.Fatal(err)
		}
	}

	runs, err := layout.RunDirs()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{scoped.RunsDir()}; !reflect.DeepEqual(runs, want) {
		t.Fatalf("RunDirs = %v, want %v", runs, want)
	}
	workcopies, err := layout.WorkcopiesDirs()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{scoped.WorkcopiesDir()}; !reflect.DeepEqual(workcopies, want) {
		t.Fatalf("WorkcopiesDirs = %v, want %v", workcopies, want)
	}
}
