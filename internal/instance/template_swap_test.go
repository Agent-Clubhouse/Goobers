package instance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/gaggletemplate"
)

func TestPreparedTemplateFirstEnrollmentHoldsConfigLock(t *testing.T) {
	layout := NewLayout(t.TempDir())
	if err := os.MkdirAll(layout.ConfigDir(), 0755); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(layout.Root, "candidate")
	if err := os.MkdirAll(filepath.Join(candidate, "gaggles", "orders", gaggletemplate.MetadataDir), 0755); err != nil {
		t.Fatal(err)
	}
	swap, err := prepareSyncedConfigDir(layout, candidate)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := swap.Rollback(); err != nil {
			t.Error(err)
		}
	}()
	if release, err := gaggletemplate.LockConfig(layout.ConfigDir()); err == nil {
		_ = release()
		t.Fatal("first enrollment did not serialize edits with the pending swap")
	}
}

func TestPreparedTemplateSourceSwapProtectsEditsAndReleasesLock(t *testing.T) {
	root := t.TempDir()
	layout := NewLayout(root)
	current := filepath.Join(layout.ConfigDir(), "gaggles", "orders")
	tree := gaggletemplate.Tree{"gaggle.yaml": {Data: []byte("spec: {displayName: Original}\n"), Mode: 0644}}
	source := gaggletemplate.Source{SchemaVersion: 1, Repository: root, Ref: "main", Directory: "templates/orders", Gaggle: "orders"}
	write := func(config string, files gaggletemplate.Tree) {
		t.Helper()
		dir := filepath.Join(config, "gaggles", "orders")
		if err := files.Write(dir); err != nil {
			t.Fatal(err)
		}
		if err := gaggletemplate.Save(dir, source, strings.Repeat("1", 40), tree); err != nil {
			t.Fatal(err)
		}
		if err := gaggletemplate.RecordDeployments(config); err != nil {
			t.Fatal(err)
		}
	}
	write(layout.ConfigDir(), tree)
	if err := os.WriteFile(filepath.Join(current, "gaggle.yaml"), []byte("spec: {displayName: Customized}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(root, "candidate")
	write(staged, tree)
	if _, err := prepareSyncedConfigDir(layout, staged); err == nil || !strings.Contains(err.Error(), "unpersisted") {
		t.Fatalf("source replacement overwrote runtime edits: %v", err)
	}
	edited, err := gaggletemplate.ReadTree(current)
	if err != nil {
		t.Fatal(err)
	}
	for _, rollback := range []bool{true, false} {
		write(staged, edited)
		swap, err := prepareSyncedConfigDir(layout, staged)
		if err != nil {
			t.Fatal(err)
		}
		if release, err := gaggletemplate.LockConfig(layout.ConfigDir()); err == nil {
			_ = release()
			t.Fatal("source swap failed to hold config lock")
		}
		if rollback {
			err = swap.Rollback()
		} else {
			err = swap.Commit()
		}
		if err != nil {
			t.Fatal(err)
		}
		release, err := gaggletemplate.LockConfig(layout.ConfigDir())
		if err != nil {
			t.Fatalf("source swap leaked config lock: %v", err)
		}
		if err := release(); err != nil {
			t.Fatal(err)
		}
		actual, err := gaggletemplate.ReadTree(current)
		if err != nil || !gaggletemplate.Equivalent(actual, edited) {
			t.Fatalf("runtime edits not preserved: %v", err)
		}
	}
}
