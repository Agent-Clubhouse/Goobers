package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadSkipInventoryParsesEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "skip-inventory.txt")
	writeFile(t, path, "# header comment\n\ngithub.com/goobers/goobers/foo # reason one\ngithub.com/goobers/goobers/bar # reason two, with a comma\n")

	pkgs, reasons, err := readSkipInventory(path)
	if err != nil {
		t.Fatalf("readSkipInventory: %v", err)
	}
	if len(pkgs) != 2 || !pkgs["github.com/goobers/goobers/foo"] || !pkgs["github.com/goobers/goobers/bar"] {
		t.Fatalf("pkgs = %v, want foo and bar", pkgs)
	}
	if reasons["github.com/goobers/goobers/foo"] != "reason one" {
		t.Errorf("reason[foo] = %q, want %q", reasons["github.com/goobers/goobers/foo"], "reason one")
	}
	if reasons["github.com/goobers/goobers/bar"] != "reason two, with a comma" {
		t.Errorf("reason[bar] = %q, want %q", reasons["github.com/goobers/goobers/bar"], "reason two, with a comma")
	}
}

func TestReadSkipInventoryMissingFileIsEmpty(t *testing.T) {
	pkgs, reasons, err := readSkipInventory(filepath.Join(t.TempDir(), "does-not-exist.txt"))
	if err != nil {
		t.Fatalf("readSkipInventory: %v", err)
	}
	if len(pkgs) != 0 || len(reasons) != 0 {
		t.Fatalf("pkgs, reasons = %v, %v; want both empty", pkgs, reasons)
	}
}

func TestReadSkipInventoryRejectsMissingReason(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "skip-inventory.txt")
	writeFile(t, path, "github.com/goobers/goobers/foo # \n")
	if _, _, err := readSkipInventory(path); err == nil {
		t.Fatal("expected an error for an entry with an empty reason")
	}
}

func TestReadSkipInventoryRejectsMalformedLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "skip-inventory.txt")
	writeFile(t, path, "github.com/goobers/goobers/foo -- missing the separator\n")
	if _, _, err := readSkipInventory(path); err == nil {
		t.Fatal("expected an error for a line with no ` # ` separator")
	}
}

func TestReadSkipInventoryRejectsDuplicateEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "skip-inventory.txt")
	writeFile(t, path, "github.com/goobers/goobers/foo # first\ngithub.com/goobers/goobers/foo # second\n")
	if _, _, err := readSkipInventory(path); err == nil {
		t.Fatal("expected an error for a duplicate package entry")
	}
}

func TestGoTestCommandAndPackageTokenExtraction(t *testing.T) {
	cases := []struct {
		name string
		run  string
		want []string
	}{
		{
			name: "single package",
			run:  "go test ./internal/platform/proc",
			want: []string{"./internal/platform/proc"},
		},
		{
			name: "multiple packages one line",
			run:  "go test ./internal/platform/durability ./internal/platform/proc ./internal/platform/secfile",
			want: []string{"./internal/platform/durability", "./internal/platform/proc", "./internal/platform/secfile"},
		},
		{
			name: "package with a -run filter afterward",
			run:  "go test ./internal/executor -run '^TestShellExecutor_RunScript$'",
			want: []string{"./internal/executor"},
		},
		{
			name: "ellipsis pattern",
			run:  "go test ./internal/readmodel/... ./internal/readservice",
			want: []string{"./internal/readmodel/...", "./internal/readservice"},
		},
		{
			name: "not a go test line at all",
			run:  "go run ./test/windowsvalidate -bin bin/goobers.exe -out bin/windows-validation-evidence",
			want: nil,
		},
		{
			name: "go build line, not go test",
			run:  "go build -o bin/goobers.exe ./cmd/goobers",
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			for _, match := range goTestCommandRE.FindAllStringSubmatch(c.run, -1) {
				got = append(got, packageTokenRE.FindAllString(match[1], -1)...)
			}
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("got %v, want %v", got, c.want)
				}
			}
		})
	}
}

func TestRepositoryRootFrom(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module github.com/goobers/goobers\n\ngo 1.26\n")
	nested := filepath.Join(root, "test", "windowscoverage")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := repositoryRootFrom(nested)
	if err != nil {
		t.Fatalf("repositoryRootFrom(nested): %v", err)
	}
	wantRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	gotRoot, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatal(err)
	}
	if gotRoot != wantRoot {
		t.Errorf("repositoryRootFrom(nested) = %q, want %q", gotRoot, wantRoot)
	}
}

func TestRepositoryRootFromRejectsUnrelatedModule(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module github.com/someone/else\n\ngo 1.26\n")
	if _, err := repositoryRootFrom(dir); err == nil {
		t.Fatal("expected an error walking from a directory whose go.mod names a different module, all the way to the filesystem root")
	}
}
