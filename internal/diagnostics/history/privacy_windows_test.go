//go:build windows

package history

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/goobers/goobers/internal/platform/secfile"
	"github.com/goobers/goobers/internal/telemetry"
)

func setHistoryTestDACL(t *testing.T, path string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
	runtime.KeepAlive(sd)
	if err != nil {
		t.Fatal(err)
	}
}
func historyTestDACL(t *testing.T, path string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return sd.String()
}
func assertHistoryPrivate(t *testing.T, path string) {
	t.Helper()
	if err := secfile.VerifyPrivate(path); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsHistoryRepairsInheritedAndProtectedACLs(t *testing.T) {
	parent := t.TempDir()
	setHistoryTestDACL(t, parent)
	beforeParent := historyTestDACL(t, parent)
	dir := filepath.Join(parent, "diagnostics")
	store := openStore(t, dir, nil)
	assertHistoryPrivate(t, dir)
	assertHistoryPrivate(t, filepath.Join(dir, lockName))
	probe := filepath.Join(dir, "inheritance-probe")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	assertHistoryPrivate(t, probe)
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	// Probe the production scratch creation boundary before any payload write.
	scratch, err := store.createScratch()
	if err != nil {
		t.Fatal(err)
	}
	info, err := scratch.Stat()
	if err != nil || info.Size() != 0 {
		t.Fatal("scratch was not empty before payload", info, err)
	}
	assertHistoryPrivate(t, filepath.Join(dir, scratchName))
	if err := scratch.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, scratchName)); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(context.Background(), []telemetry.DiagnosticRecord{validRecord(time.Now().UTC())}); err != nil {
		t.Fatal(err)
	}
	assertHistoryPrivate(t, filepath.Join(dir, snapshotName))
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// Protected child ACLs cannot rely on directory inheritance for repair.
	for _, path := range []string{dir, filepath.Join(dir, snapshotName), filepath.Join(dir, lockName)} {
		setHistoryTestDACL(t, path)
		if err := secfile.VerifyPrivate(path); err == nil {
			t.Fatal("permissive fixture was already private", path)
		}
	}
	restarted := openStore(t, dir, nil)
	for _, path := range []string{dir, filepath.Join(dir, snapshotName), filepath.Join(dir, lockName)} {
		assertHistoryPrivate(t, path)
	}
	if err := restarted.Append(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if after := historyTestDACL(t, parent); after != beforeParent {
		t.Fatal("parent ACL changed outside diagnostic store")
	}
	snapshot, err := Read(dir)
	if err != nil || len(snapshot.Records) != 1 {
		t.Fatal(snapshot, err)
	}
}

func TestWindowsHistoryUnsafeAliasesDoNotChangeOutsideACLs(t *testing.T) {
	for _, name := range []string{snapshotName, lockName} {
		for _, kind := range []string{"hardlink", "symlink"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				dir := t.TempDir()
				outside := filepath.Join(t.TempDir(), "outside")
				if err := os.WriteFile(outside, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				setHistoryTestDACL(t, outside)
				before := historyTestDACL(t, outside)
				link := filepath.Join(dir, name)
				var err error
				if kind == "hardlink" {
					err = os.Link(outside, link)
				} else {
					err = os.Symlink(outside, link)
				}
				if err != nil {
					t.Fatal("native alias fixture required", err)
				}
				if store, err := Open(context.Background(), dir, nil); err == nil {
					_ = store.Close()
					t.Fatal("unsafe alias accepted")
				}
				if after := historyTestDACL(t, outside); after != before {
					t.Fatal("outside ACL changed")
				}
				data, err := os.ReadFile(outside)
				if err != nil || len(data) != 0 {
					t.Fatal("outside content changed", err)
				}
			})
		}
	}
}

func TestWindowsHistoryDirectoryAliasLeavesOutsideUntouched(t *testing.T) {
	outside := t.TempDir()
	setHistoryTestDACL(t, outside)
	before := historyTestDACL(t, outside)
	alias := filepath.Join(t.TempDir(), "diagnostics")
	if err := os.Symlink(outside, alias); err != nil {
		t.Fatal("native directory alias fixture required", err)
	}
	if store, err := Open(context.Background(), alias, nil); err == nil {
		_ = store.Close()
		t.Fatal("directory alias accepted")
	}
	if after := historyTestDACL(t, outside); after != before {
		t.Fatal("outside directory ACL changed")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatal("outside directory contents changed", err)
	}
}
