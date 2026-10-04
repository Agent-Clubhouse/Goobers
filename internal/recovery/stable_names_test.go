package recovery

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestListStableNamesSortsAndAcceptsEOF(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"charlie", "alpha", "bravo"} {
		if err := os.WriteFile(filepath.Join(root, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	names, err := listStableNames(root, before, listNamesOptions{
		readLimit:        4,
		changedRootError: "root changed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"alpha", "bravo", "charlie"}; !slices.Equal(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
}

func TestListStableNamesRejectsChangedRoot(t *testing.T) {
	before, err := os.Lstat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = listStableNames(t.TempDir(), before, listNamesOptions{
		readLimit:        1,
		changedRootError: "root changed while opening",
	})
	if err == nil || err.Error() != "root changed while opening" {
		t.Fatalf("changed root error = %v", err)
	}
}

func TestListStableNamesIgnoresConfiguredNamesForCap(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{".inventory.lock", "reservation"} {
		if err := os.WriteFile(filepath.Join(root, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	names, err := listStableNames(root, before, listNamesOptions{
		readLimit:        3,
		changedRootError: "root changed",
		ignoredForCount:  []string{".inventory.lock"},
		maxCount:         1,
		fullError: func(count int) error {
			return fmt.Errorf("%w: %d entries", ErrInventoryFull, count)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{".inventory.lock", "reservation"}; !slices.Equal(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
}

func TestListStableNamesReturnsConfiguredFullError(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"first", "second"} {
		if err := os.WriteFile(filepath.Join(root, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = listStableNames(root, before, listNamesOptions{
		readLimit:        2,
		changedRootError: "root changed",
		maxCount:         1,
		fullError: func(count int) error {
			return fmt.Errorf("%w: counted %d", ErrInventoryFull, count)
		},
	})
	if !errors.Is(err, ErrInventoryFull) || err.Error() != "recovery inventory is full: counted 2" {
		t.Fatalf("full error = %v", err)
	}
}
