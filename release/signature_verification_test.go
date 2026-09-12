package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/testgit"
)

const (
	releaseSignaturePrincipal   = "scratch@local"
	releaseSignatureFingerprint = "SHA256:FhUIcb6GKaWdMLpoeizCO0kCeaI68W5BCtYm3XHS4Qs"
	releaseVerificationRecipe   = `TAG=v0.4.0-rc.2
git fetch origin "refs/tags/${TAG}:refs/tags/${TAG}"
git -c gpg.ssh.allowedSignersFile=.github/allowed_signers tag -v "${TAG}"`
)

func gitCommand(t *testing.T, directory string, args ...string) []byte {
	t.Helper()
	command := testgit.Command(args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return bytes.TrimSpace(output)
}

// The allowed-signers file is a release trust root, not explanatory prose.
// Verify it against the exact published tag object from which the key was
// recovered so a stale key, principal, namespace, or recipe fails CI.
func TestPublishedReleaseKeyVerifiesKnownTag(t *testing.T) {
	repository := t.TempDir()
	gitCommand(t, repository, "init", "--quiet")
	fixture, err := filepath.Abs("testdata/v0.4.0-rc.2.tag")
	if err != nil {
		t.Fatal(err)
	}
	objectID := string(gitCommand(t, repository, "hash-object", "-t", "tag", "-w", fixture))
	allowedSigners, err := filepath.Abs("../.github/allowed_signers")
	if err != nil {
		t.Fatal(err)
	}
	verification := string(gitCommand(t, repository,
		"-c", "gpg.ssh.allowedSignersFile="+allowedSigners,
		"verify-tag", objectID,
	))
	for _, want := range []string{releaseSignaturePrincipal, releaseSignatureFingerprint} {
		if !strings.Contains(verification, want) {
			t.Fatalf("verification output %q does not identify %q", verification, want)
		}
	}

	tagObject, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Replace(tagObject, []byte("Second release candidate"), []byte("Forged release candidate"), 1)
	tamperedPath := filepath.Join(repository, "tampered.tag")
	if err := os.WriteFile(tamperedPath, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	tamperedID := string(gitCommand(t, repository, "hash-object", "-t", "tag", "-w", tamperedPath))
	command := testgit.Command("-c", "gpg.ssh.allowedSignersFile="+allowedSigners, "verify-tag", tamperedID)
	command.Dir = repository
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("tampered tag verified successfully:\n%s", output)
	}
}

func TestReleaseVerificationDocsKeepTheRunnableContract(t *testing.T) {
	files := []string{"../SECURITY.md", "../docs/guides/releases.md"}
	for _, file := range files {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			".github/allowed_signers",
			releaseVerificationRecipe,
			"tag annotation",
		} {
			if !bytes.Contains(body, []byte(want)) {
				t.Errorf("%s does not preserve the release-verification contract %q", file, want)
			}
		}
	}
}
