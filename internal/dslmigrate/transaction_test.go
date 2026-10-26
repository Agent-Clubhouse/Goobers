package dslmigrate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func transactionFixture(t *testing.T) (string, []FileChange) {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "config"), 0700); err != nil {
		t.Fatal(err)
	}
	changes := []FileChange{{Path: "one.yaml", Before: "# exact\r\ndslVersion: '2.0'\r\n", After: "# exact\r\ndslVersion: '2.1'\r\n"}, {Path: "two.yaml", Before: "dslVersion: 2.0", After: "dslVersion: 2.1"}}
	for _, c := range changes {
		if err := os.WriteFile(filepath.Join(root, "config", c.Path), []byte(c.Before), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root, changes
}

func assertTransactionOriginals(t *testing.T, root string, changes []FileChange) {
	t.Helper()
	for _, c := range changes {
		got, err := os.ReadFile(filepath.Join(root, "config", c.Path))
		if err != nil || string(got) != c.Before {
			t.Fatalf("original %s not preserved: %q %v", c.Path, got, err)
		}
	}
}

func TestBackupRollbackRoundTripAndBound(t *testing.T) {
	root, changes := transactionFixture(t)
	if err := WriteWithBackup(root, changes); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, BackupName)); err != nil {
		t.Fatal(err)
	}
	if err := Rollback(root); err != nil {
		t.Fatal(err)
	}
	assertTransactionOriginals(t, root, changes)
	if _, err := os.Stat(filepath.Join(root, BackupName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rollback did not consume backup")
	}
	if err := WriteWithBackup(root, append(changes, make([]FileChange, maxBackupFiles)...)); err == nil {
		t.Fatal("unbounded file count")
	}
	assertTransactionOriginals(t, root, changes)
	changes[0].Before = strings.Repeat("x", maxBackupBytes)
	if err := os.WriteFile(filepath.Join(root, "config", changes[0].Path), []byte(changes[0].Before), 0600); err != nil {
		t.Fatal(err)
	}
	if err := WriteWithBackup(root, changes); err == nil {
		t.Fatal("unbounded backup size")
	}
	assertTransactionOriginals(t, root, changes)
}

func TestPartialWriteRetainsRecoverableTransaction(t *testing.T) {
	root, changes := transactionFixture(t)
	calls := 0
	err := writeWithBackup(root, changes, func(path string, data []byte, mode os.FileMode) error {
		calls++
		if calls == 2 {
			return errors.New("injected storage failure")
		}
		return journal.WriteFileAtomic(path, data, mode)
	})
	if err == nil || !strings.Contains(err.Error(), "backup retained") {
		t.Fatalf("missing recovery instruction: %v", err)
	}
	if err := Rollback(root); err != nil {
		t.Fatal(err)
	}
	assertTransactionOriginals(t, root, changes)
}

func TestRollbackRefusesUserEditsBeforeAnyRestore(t *testing.T) {
	root, changes := transactionFixture(t)
	if err := WriteWithBackup(root, changes); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config", changes[1].Path)
	if err := os.WriteFile(path, []byte("user change"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Rollback(root); err == nil {
		t.Fatal("overwrote user changes")
	}
	first, _ := os.ReadFile(filepath.Join(root, "config", changes[0].Path))
	if string(first) != changes[0].After {
		t.Fatal("partial rollback before checking all destinations")
	}
	current, _ := os.ReadFile(path)
	if string(current) != "user change" {
		t.Fatal("user edit changed")
	}
}

func TestBackupCollisionAndChangedSourcePreventWrites(t *testing.T) {
	root, changes := transactionFixture(t)
	if err := os.WriteFile(filepath.Join(root, BackupName), []byte("existing backup"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := WriteWithBackup(root, changes); err == nil {
		t.Fatal("overwrote backup")
	}
	assertTransactionOriginals(t, root, changes)
	if err := os.Remove(filepath.Join(root, BackupName)); err != nil {
		t.Fatal(err)
	}
	changes[1].Before = "stale preview"
	if err := WriteWithBackup(root, changes); err == nil {
		t.Fatal("accepted stale source")
	}
	first, _ := os.ReadFile(filepath.Join(root, "config", changes[0].Path))
	if string(first) != changes[0].Before {
		t.Fatal("wrote before preflight completed")
	}
}

func TestRollbackRefusesCorruptOrUnsafeBackup(t *testing.T) {
	for _, damage := range []func(string) string{
		func(s string) string { return strings.Replace(s, "one.yaml", "../x.yaml", 1) },
		func(s string) string { return strings.Replace(s, "exact", "corrupt", 1) },
		func(s string) string { return s + "{}" },
		func(s string) string { return strings.Replace(s, `"version":1`, `"version":2`, 1) },
	} {
		root, changes := transactionFixture(t)
		if err := WriteWithBackup(root, changes); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, BackupName)
		raw, _ := os.ReadFile(path)
		if err := os.WriteFile(path, []byte(damage(string(raw))), 0600); err != nil {
			t.Fatal(err)
		}
		if err := Rollback(root); err == nil {
			t.Fatal("accepted damaged backup")
		}
		for _, c := range changes {
			current, _ := os.ReadFile(filepath.Join(root, "config", c.Path))
			if string(current) != c.After {
				t.Fatal("damaged backup changed sources")
			}
		}
	}
}

func TestTransactionRejectsSymlinkAndUnsafePaths(t *testing.T) {
	root, changes := transactionFixture(t)
	for _, unsafe := range []string{"../one.yaml", "/one.yaml", "nested/../one.yaml", `nested\one.yaml`, "C:one.yaml"} {
		change := changes[0]
		change.Path = unsafe
		if err := WriteWithBackup(root, []FileChange{change}); err == nil {
			t.Fatalf("accepted %q", unsafe)
		}
	}
	source := filepath.Join(root, "config", changes[0].Path)
	target := filepath.Join(root, "outside.yaml")
	if err := os.Rename(source, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, source); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := WriteWithBackup(root, changes); err == nil {
		t.Fatal("followed symlink")
	}
	current, _ := os.ReadFile(target)
	if string(current) != changes[0].Before {
		t.Fatal("changed symlink target")
	}
}

func TestTransactionNestedPortablePaths(t *testing.T) {
	root, changes := transactionFixture(t)
	if err := os.MkdirAll(filepath.Join(root, "config", "nested", "workflows"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "config", changes[0].Path), filepath.Join(root, "config", "nested", "workflows", "one.yaml")); err != nil {
		t.Fatal(err)
	}
	changes[0].Path = "nested/workflows/one.yaml"
	if err := WriteWithBackup(root, changes); err != nil {
		t.Fatal(err)
	}
	if err := Rollback(root); err != nil {
		t.Fatal(err)
	}
	assertTransactionOriginals(t, root, changes)
}
