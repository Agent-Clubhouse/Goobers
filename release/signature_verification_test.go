package main

import (
	"bytes"
	"crypto/sha512"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/testgit"
	"golang.org/x/crypto/ssh"
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

type sshSignatureEnvelope struct {
	Version       uint32
	PublicKey     []byte
	Namespace     string
	Reserved      string
	HashAlgorithm string
	Signature     []byte
}

type sshSignedPayload struct {
	Namespace     string
	Reserved      string
	HashAlgorithm string
	Hash          []byte
}

func publishedReleaseKey(t *testing.T, allowedSigners []byte) ssh.PublicKey {
	t.Helper()
	for line := range strings.SplitSeq(string(allowedSigners), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 4 || fields[0] != releaseSignaturePrincipal || fields[1] != `namespaces="git"` {
			t.Fatalf("unexpected release allowed-signers entry %q", line)
		}
		key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(fields[2] + " " + fields[3]))
		if err != nil {
			t.Fatalf("parse published release key: %v", err)
		}
		if got := ssh.FingerprintSHA256(key); got != releaseSignatureFingerprint {
			t.Fatalf("published release key fingerprint = %q, want %q", got, releaseSignatureFingerprint)
		}
		return key
	}
	t.Fatal("published release key is missing")
	return nil
}

func verifyTagSSHSIG(tagObject []byte, trustedKey ssh.PublicKey) error {
	const (
		armor = "-----BEGIN SSH SIGNATURE-----"
		magic = "SSHSIG"
	)
	marker := bytes.Index(tagObject, []byte(armor))
	if marker < 0 {
		return fmt.Errorf("SSH signature armor is missing")
	}
	block, rest := pem.Decode(tagObject[marker:])
	if block == nil || block.Type != "SSH SIGNATURE" || len(bytes.TrimSpace(rest)) != 0 {
		return fmt.Errorf("invalid SSH signature armor")
	}
	if !bytes.HasPrefix(block.Bytes, []byte(magic)) {
		return fmt.Errorf("invalid SSH signature magic")
	}
	var envelope sshSignatureEnvelope
	if err := ssh.Unmarshal(block.Bytes[len(magic):], &envelope); err != nil {
		return fmt.Errorf("decode SSH signature: %w", err)
	}
	if envelope.Version != 1 || envelope.Namespace != "git" || envelope.Reserved != "" {
		return fmt.Errorf("unexpected SSH signature envelope")
	}
	if envelope.HashAlgorithm != "sha512" {
		return fmt.Errorf("unsupported SSH signature hash %q", envelope.HashAlgorithm)
	}
	signer, err := ssh.ParsePublicKey(envelope.PublicKey)
	if err != nil {
		return fmt.Errorf("parse SSH signature key: %w", err)
	}
	if !bytes.Equal(signer.Marshal(), trustedKey.Marshal()) {
		return fmt.Errorf("SSH signature key is not the published release key")
	}
	var signature ssh.Signature
	if err := ssh.Unmarshal(envelope.Signature, &signature); err != nil {
		return fmt.Errorf("decode SSH signature blob: %w", err)
	}
	digest := sha512.Sum512(tagObject[:marker])
	signed := append([]byte(magic), ssh.Marshal(sshSignedPayload{
		Namespace: envelope.Namespace, HashAlgorithm: envelope.HashAlgorithm, Hash: digest[:],
	})...)
	if err := trustedKey.Verify(signed, &signature); err != nil {
		return fmt.Errorf("verify SSH signature: %w", err)
	}
	return nil
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
	if objectID != "2cde3bd1abda5fb3bc854cc98219a96c7fae42f0" {
		t.Fatalf("published tag fixture object ID = %s", objectID)
	}
	allowedSignersPath, err := filepath.Abs("../.github/allowed_signers")
	if err != nil {
		t.Fatal(err)
	}
	allowedSigners, err := os.ReadFile(allowedSignersPath)
	if err != nil {
		t.Fatal(err)
	}
	tagObject, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	trustedKey := publishedReleaseKey(t, allowedSigners)
	if err := verifyTagSSHSIG(tagObject, trustedKey); err != nil {
		t.Fatalf("verify published tag object %s: %v", objectID, err)
	}

	tampered := bytes.Replace(tagObject, []byte("Second release candidate"), []byte("Forged release candidate"), 1)
	if err := verifyTagSSHSIG(tampered, trustedKey); err == nil {
		t.Fatal("tampered tag verified successfully")
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
