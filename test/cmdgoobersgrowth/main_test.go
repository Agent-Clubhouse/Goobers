package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/testgit"
)

func put(t *testing.T, root, path, content string) {
	t.Helper()
	path = filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := testgit.Command(append([]string{"-C", root}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	put(t, root, "cmd/goobers/a.go", "package main\n\n// a\n")
	put(t, root, "cmd/goobers/b.go", "package main\n\n// b\n")
	put(t, root, baselinePath, "!non-test-line-count 6\n!non-test-file-count 2\n")
	git(t, root, "init")
	git(t, root, "add", ".")
	git(t, root, "-c", "user.name=Gate Test", "-c", "user.email=gate@example.invalid", "-c", "commit.gpgsign=false", "commit", "-m", "baseline")
	return root
}

func invoke(root string, args ...string) (int, string) {
	var output bytes.Buffer
	status := run(append([]string{"-root", root}, args...), &output, &output)
	return status, output.String()
}

func TestUpdateRequiresExactTargetForBothDimensions(t *testing.T) {
	root := fixture(t)
	put(t, root, "cmd/goobers/c.go", "package main\n")
	original, err := os.ReadFile(filepath.Join(root, baselinePath))
	if err != nil {
		t.Fatal(err)
	}
	if code, output := invoke(root, "-update"); code == 0 {
		t.Fatalf("unjustified update passed: %s", output)
	}
	after, err := os.ReadFile(filepath.Join(root, baselinePath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		t.Fatal("failed update modified baseline")
	}
	put(t, root, baselinePath, string(original)+"!non-test-line-count-justification\t7\tCLI adapter\n!non-test-file-count-justification\t3\tCLI adapter\n")
	if code, output := invoke(root, "-update"); code != 0 {
		t.Fatalf("justified update failed: %s", output)
	}
	if code, output := invoke(root); code != 0 {
		t.Fatalf("updated gate failed: %s", output)
	}
	base, err := readBaseline(filepath.Join(root, baselinePath))
	if err != nil {
		t.Fatal(err)
	}
	if base.counts[dimensions[0]] != 7 || base.counts[dimensions[1]] != 3 || len(base.reasons) != 2 {
		t.Fatalf("wrong re-pin: %+v", base)
	}
	if err := os.Remove(filepath.Join(root, "cmd/goobers/c.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "cmd/goobers/b.go")); err != nil {
		t.Fatal(err)
	}
	if code, output := invoke(root, "-update"); code != 0 {
		t.Fatalf("decrease update failed: %s", output)
	}
	base, err = readBaseline(filepath.Join(root, baselinePath))
	if err != nil {
		t.Fatal(err)
	}
	if base.counts[dimensions[0]] != 3 || base.counts[dimensions[1]] != 1 || len(base.reasons) != 0 {
		t.Fatalf("wrong tightened baseline: %+v", base)
	}
}

func TestScanPackage(t *testing.T) {
	root := fixture(t)
	put(t, root, "cmd/goobers/ignored_test.go", "many\nextra\nlines\n")
	put(t, root, "cmd/goobers/nested/other.go", "package other\n")
	put(t, root, "cmd/goobers/readme.md", "ignored\n")
	put(t, root, "cmd/goobers/platform_windows.go", "//go:build windows\r\npackage main")
	counts, err := scanPackage(root)
	if err != nil {
		t.Fatal(err)
	}
	if counts[dimensions[0]] != 8 || counts[dimensions[1]] != 3 {
		t.Fatalf("counts: %v", counts)
	}
}

func TestInvalidBaselines(t *testing.T) {
	valid := "!non-test-line-count 6\n!non-test-file-count 2\n"
	for _, content := range []string{"", "!non-test-line-count 6\n", valid + "!non-test-file-count 3\n", strings.Replace(valid, "6", "-1", 1), strings.Replace(valid, "6", "bad", 1), valid + "!unknown 1\n", valid + "!non-test-file-count-justification\t3\t\n", valid + "!non-test-file-count-justification\tbad\treason\n", valid + strings.Repeat("!non-test-file-count-justification\t3\treason\n", 2)} {
		if _, err := parseBaseline([]byte(content)); err == nil {
			t.Errorf("accepted %q", content)
		}
	}
}

func TestMissingBaselinesAndInvalidBaseReference(t *testing.T) {
	root := fixture(t)
	if code, _ := invoke(root, "-update", "-base-ref", "missing-revision"); code == 0 {
		t.Fatal("missing base revision accepted")
	}
	if code, _ := invoke(root, "-update", "-base-ref", ""); code == 0 {
		t.Fatal("empty base revision accepted")
	}
	if err := os.Remove(filepath.Join(root, baselinePath)); err != nil {
		t.Fatal(err)
	}
	if code, output := invoke(root); code != 0 {
		t.Fatalf("informational main report requires a baseline: %s", output)
	}
	// Removing the working baseline cannot bypass a committed ceiling.
	put(t, root, "cmd/goobers/c.go", "package main\n")
	if code, _ := invoke(root, "-update"); code == 0 {
		t.Fatal("missing candidate allowed unqualified growth")
	}
}
