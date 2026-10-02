package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
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

func TestBothRatchetDimensions(t *testing.T) {
	for _, dimension := range dimensions {
		t.Run(dimension, func(t *testing.T) {
			for _, scenario := range []string{"unchanged", "growth", "decrease", "repin-unjustified", "repin-stale", "repin-justified", "committed-unjustified", "committed-justified"} {
				t.Run(scenario, func(t *testing.T) {
					root := fixture(t)
					baseRef := git(t, root, "rev-parse", "HEAD")
					old, next := 6, 7
					if dimension == dimensions[1] {
						old, next = 2, 3
					}
					if scenario != "unchanged" {
						if dimension == dimensions[0] {
							source := "package main\n\n// a\n\n"
							if scenario == "decrease" {
								source = "package main\n\n"
							}
							put(t, root, "cmd/goobers/a.go", source)
						} else if scenario == "decrease" {
							if err := os.Remove(filepath.Join(root, "cmd/goobers/b.go")); err != nil {
								t.Fatal(err)
							}
						} else {
							// An empty file isolates file growth from line growth.
							put(t, root, "cmd/goobers/c.go", "")
						}
					}
					if strings.Contains(scenario, "repin") || strings.HasPrefix(scenario, "committed") {
						lines, files := 6, 2
						if dimension == dimensions[0] {
							lines = next
						} else {
							files = next
						}
						content := fmt.Sprintf("!non-test-line-count %d\n!non-test-file-count %d\n", lines, files)
						if scenario == "repin-stale" {
							content += fmt.Sprintf("!%s-justification\t%d\tprevious exception\n", dimension, old)
						}
						if scenario == "repin-justified" || scenario == "committed-justified" {
							content += fmt.Sprintf("!%s-justification\t%d\trequired CLI adapter\n", dimension, next)
						}
						put(t, root, baselinePath, content)
					}
					if strings.HasPrefix(scenario, "committed") {
						git(t, root, "add", ".")
						git(t, root, "-c", "user.name=Gate Test", "-c", "user.email=gate@example.invalid", "-c", "commit.gpgsign=false", "commit", "-m", "repin")
					}
					code, output := invoke(root, "-base-ref", baseRef)
					wantFailure := scenario == "growth" || scenario == "repin-unjustified" || scenario == "repin-stale" || scenario == "committed-unjustified"
					if (code != 0) != wantFailure {
						t.Fatalf("status %d: %s", code, output)
					}
					if wantFailure {
						for _, want := range []string{dimension, baselinePath, fmt.Sprintf("baseline %d", old), fmt.Sprintf("new value %d", next), "make cmdgoobers-growth-update"} {
							if !strings.Contains(output, want) {
								t.Errorf("output missing %q: %s", want, output)
							}
						}
					}
				})
			}
		})
	}
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
	if code, _ := invoke(root, "-base-ref", "missing-revision"); code == 0 {
		t.Fatal("missing base revision accepted")
	}
	if code, _ := invoke(root, "-base-ref", ""); code == 0 {
		t.Fatal("empty base revision accepted")
	}
	if err := os.Remove(filepath.Join(root, baselinePath)); err != nil {
		t.Fatal(err)
	}
	if code, _ := invoke(root); code == 0 {
		t.Fatal("missing current baseline accepted")
	}
	// Removing the working baseline cannot bypass a committed ceiling.
	put(t, root, "cmd/goobers/c.go", "package main\n")
	if code, _ := invoke(root, "-update"); code == 0 {
		t.Fatal("missing candidate allowed unqualified growth")
	}
}
