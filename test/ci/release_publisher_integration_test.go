//go:build integration

package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func publisherVerificationPython(t *testing.T) string {
	t.Helper()
	script := releasePublisherScript(t)
	_, body, ok := strings.Cut(script, "python3 -I - <<'PYVERIFY'\n")
	if !ok {
		t.Fatal("missing isolated publisher verifier")
	}
	body, _, ok = strings.Cut(body, "\nPYVERIFY\n")
	if !ok {
		t.Fatal("missing publisher verifier terminator")
	}
	return body
}

func publisherFixture(t *testing.T, tag string) (string, []string) {
	t.Helper()
	root := t.TempDir()
	for _, directory := range []string{"dist", "release-notes"} {
		if err := os.Mkdir(filepath.Join(root, directory), 0700); err != nil {
			t.Fatal(err)
		}
	}
	names := []string{"install.sh", "feature-registry.json", "dsl-support-matrix.json", "goobers_portal_" + tag + ".tar.gz", "goobers-agent-toolkit_" + tag + ".zip", "goobers-onboarding_" + tag + ".zip"}
	for _, target := range []string{"linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64"} {
		names = append(names, "goobers_"+tag+"_"+target+".tar.gz")
	}
	names = append(names, "goobers_"+tag+"_windows_amd64.zip")
	var manifest strings.Builder
	for _, name := range names {
		data := []byte("#!/bin/sh\ntouch PRODUCT_WAS_EXECUTED\n" + name)
		writePublisherFixture(t, root, "dist/"+name, data)
		fmt.Fprintf(&manifest, "%x  %s\n", sha256.Sum256(data), name)
	}
	writePublisherFixture(t, root, "dist/SHA256SUMS", []byte(manifest.String()))
	writePublisherFixture(t, root, "dist/RELEASE_NOTES.md", []byte("original notes"))
	writePublisherFixture(t, root, "release-notes/RELEASE_NOTES.md", []byte("Generated notes with inert $(touch NOTES_WERE_EXECUTED) shell text."))
	// A non-isolated Python import would run this artifact/repository module.
	writePublisherFixture(t, root, "hashlib.py", []byte("raise RuntimeError('repository Python module executed')\n"))
	return root, names
}

func writePublisherFixture(t *testing.T, root, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), data, 0700); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationReleasePublisherTreatsAssetsAsData(t *testing.T) {
	testdep.Require(t, "python3")
	script := publisherVerificationPython(t)
	for _, tag := range []string{"v0.4.0", "v0.4.0-rc.1"} {
		root, names := publisherFixture(t, tag)
		command := exec.Command("python3", "-I", "-")
		command.Dir, command.Stdin = root, strings.NewReader(script)
		command.Env = append(os.Environ(), "TAG="+tag, "PYTHONPATH="+root)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("valid inert release refused: %v\n%s", err, output)
		}
		data, err := os.ReadFile(filepath.Join(root, "publish-assets.txt"))
		if err != nil {
			t.Fatal(err)
		}
		actual := strings.Fields(string(data))
		for index := range actual {
			actual[index] = filepath.ToSlash(actual[index])
		}
		var expected []string
		for _, name := range names {
			expected = append(expected, "dist/"+name)
		}
		expected = append(expected, "dist/SHA256SUMS", "release-notes/RELEASE_NOTES.md")
		slices.Sort(actual)
		slices.Sort(expected)
		if !slices.Equal(actual, expected) {
			t.Fatalf("upload set differs: %v", actual)
		}
		for _, marker := range []string{"PRODUCT_WAS_EXECUTED", "NOTES_WERE_EXECUTED"} {
			if _, err := os.Stat(filepath.Join(root, marker)); !os.IsNotExist(err) {
				t.Fatalf("release data executed: %s", marker)
			}
		}
	}
}

func TestIntegrationReleasePublisherRejectsAlteredOrAmbiguousAssets(t *testing.T) {
	testdep.Require(t, "python3")
	script := publisherVerificationPython(t)
	for _, mutation := range []string{"tampered", "missing file", "extra file", "duplicate digest", "missing digest", "extra digest", "traversal digest", "empty notes", "extra notes", "oversized notes", "oversized manifest", "directory", "bad tag"} {
		t.Run(mutation, func(t *testing.T) {
			tag := "v0.4.0-rc.1"
			root, _ := publisherFixture(t, tag)
			manifestPath := filepath.Join(root, "dist", "SHA256SUMS")
			manifest, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.SplitAfter(string(manifest), "\n")
			switch mutation {
			case "tampered":
				writePublisherFixture(t, root, "dist/install.sh", []byte("changed after signing"))
			case "missing file":
				if err := os.Remove(filepath.Join(root, "dist", "install.sh")); err != nil {
					t.Fatal(err)
				}
			case "extra file":
				writePublisherFixture(t, root, "dist/unverified.exe", []byte("extra executable"))
			case "duplicate digest":
				writePublisherFixture(t, root, "dist/SHA256SUMS", append(manifest, []byte(lines[0])...))
			case "missing digest":
				writePublisherFixture(t, root, "dist/SHA256SUMS", []byte(strings.Join(lines[1:], "")))
			case "extra digest":
				writePublisherFixture(t, root, "dist/SHA256SUMS", append(manifest, []byte(strings.Repeat("a", 64)+"  unexpected.exe\n")...))
			case "traversal digest":
				writePublisherFixture(t, root, "dist/SHA256SUMS", []byte(strings.Replace(string(manifest), "  install.sh", "  ../install.sh", 1)))
			case "empty notes":
				writePublisherFixture(t, root, "release-notes/RELEASE_NOTES.md", nil)
			case "extra notes":
				writePublisherFixture(t, root, "release-notes/unverified.exe", []byte("extra"))
			case "oversized notes":
				writePublisherFixture(t, root, "release-notes/RELEASE_NOTES.md", []byte(strings.Repeat("a", (1<<20)+1)))
			case "oversized manifest":
				writePublisherFixture(t, root, "dist/SHA256SUMS", []byte(strings.Repeat("a", 65537)))
			case "directory":
				if err := os.Remove(filepath.Join(root, "dist", "install.sh")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(root, "dist", "install.sh"), 0700); err != nil {
					t.Fatal(err)
				}
			case "bad tag":
				tag = "v0.4.0;touch TAG_WAS_EXECUTED"
			}
			command := exec.Command("python3", "-I", "-")
			command.Dir, command.Stdin = root, strings.NewReader(script)
			command.Env = append(os.Environ(), "TAG="+tag)
			if output, err := command.CombinedOutput(); err == nil {
				t.Fatalf("ambiguous artifact accepted: %s", output)
			}
			if _, err := os.Stat(filepath.Join(root, "publish-assets.txt")); !os.IsNotExist(err) {
				t.Fatal("failed verification left upload authorization")
			}
		})
	}
}
