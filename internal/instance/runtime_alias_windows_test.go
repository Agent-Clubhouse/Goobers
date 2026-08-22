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
	if err := CreateLegacyRuntimeAlias(alias, target); err != nil {
		t.Fatalf("CreateLegacyRuntimeAlias: %v", err)
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

func TestLegacyRuntimeJunctionSurvivesStartupLifecycle(t *testing.T) {
	layout := NewLayout(t.TempDir())
	legacyRun := filepath.Join(layout.RunsDir(), "run-1", "run.yaml")
	legacyWorkcopy := filepath.Join(layout.WorkcopiesDir(), "repo", "repo.git", "HEAD")
	for path, contents := range map[string]string{
		legacyRun:      "runId: test\n",
		legacyWorkcopy: "ref: refs/heads/main\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	migration, err := layout.MigrateLegacyRuntimeWithReport([]string{"alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.CompleteLegacyRuntimeMigration(migration); err != nil {
		t.Fatal(err)
	}
	if _, err := layout.MigrateLegacyRuntimeWithReport([]string{"alpha"}); err != nil {
		t.Fatalf("restart migration: %v", err)
	}

	scoped := layout.ForGaggle("alpha")
	for _, alias := range []string{layout.RunsDir(), layout.WorkcopiesDir()} {
		info, err := os.Lstat(alias)
		if err != nil {
			t.Fatal(err)
		}
		isAlias, err := isLegacyRuntimeAlias(alias, info)
		if err != nil {
			t.Fatal(err)
		}
		if !isAlias {
			t.Fatalf("%s was not recognized as a legacy runtime alias", alias)
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
