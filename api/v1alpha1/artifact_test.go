package v1alpha1

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteAndResolveArtifactRoundTrip(t *testing.T) {
	root := t.TempDir()
	content := []byte("token-bucket rate limiter, 100/min\n")

	ptr, err := WriteArtifact(root, "artifacts/impl/notes.txt", content, "text/plain")
	if err != nil {
		t.Fatalf("WriteArtifact: %v", err)
	}
	if ptr.Digest != Digest(content) {
		t.Errorf("digest = %q, want %q", ptr.Digest, Digest(content))
	}
	if ptr.Size != int64(len(content)) {
		t.Errorf("size = %d, want %d", ptr.Size, len(content))
	}
	if err := ptr.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}

	got, err := ptr.Resolve(root)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("resolved %q, want %q", got, content)
	}
}

func TestResolveDetectsTampering(t *testing.T) {
	root := t.TempDir()
	ptr, err := WriteArtifact(root, "artifacts/impl/notes.txt", []byte("original"), "")
	if err != nil {
		t.Fatalf("WriteArtifact: %v", err)
	}
	// Tamper with the bytes on disk; the pointer's digest no longer matches.
	if err := os.WriteFile(filepath.Join(root, ptr.Path), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = ptr.Resolve(root)
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("Resolve after tamper: got %v, want ErrDigestMismatch", err)
	}
}

func TestArtifactPathContainment(t *testing.T) {
	root := t.TempDir()
	for _, bad := range []string{"../escape.txt", "/etc/passwd", "a/../../b", ""} {
		if _, err := WriteArtifact(root, bad, []byte("x"), ""); !errors.Is(err, ErrPathEscape) {
			t.Errorf("WriteArtifact(%q): got %v, want ErrPathEscape", bad, err)
		}
		p := ArtifactPointer{Path: bad, Digest: Digest([]byte("x"))}
		if _, err := p.Resolve(root); err == nil {
			t.Errorf("Resolve(%q) unexpectedly succeeded", bad)
		} else if bad != "" && !errors.Is(err, ErrPathEscape) {
			t.Errorf("Resolve(%q): got %v, want ErrPathEscape", bad, err)
		}
	}
}

func TestContextPointerValidate(t *testing.T) {
	good := Digest([]byte("x"))
	cases := []struct {
		name string
		cp   ContextPointer
		ok   bool
	}{
		{"artifact-only", ContextPointer{Name: "a", Artifact: &ArtifactPointer{Path: "artifacts/x", Digest: good}}, true},
		{"external-only", ContextPointer{Name: "b", External: &ExternalRef{Kind: "url", URI: "https://x"}}, true},
		{"no-name", ContextPointer{Artifact: &ArtifactPointer{Path: "artifacts/x", Digest: good}}, false},
		{"neither", ContextPointer{Name: "c"}, false},
		{"both", ContextPointer{Name: "d", Artifact: &ArtifactPointer{Path: "artifacts/x", Digest: good}, External: &ExternalRef{Kind: "url", URI: "https://x"}}, false},
		{"external-no-uri", ContextPointer{Name: "e", External: &ExternalRef{Kind: "url"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cp.Validate()
			if tc.ok && err != nil {
				t.Errorf("expected valid, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Error("expected invalid, got nil")
			}
		})
	}
}

// TestTwoStagePipelineByPointerOnly is issue #10's headline acceptance check: a
// toy two-stage pipeline where stage A produces an artifact and stage B consumes
// it BY POINTER ONLY — B never receives A's ResultEnvelope, only a ContextPointer
// it resolves read-only from the journal, digest-verified.
func TestTwoStagePipelineByPointerOnly(t *testing.T) {
	journal := t.TempDir() // stands in for runs/<run-id>/

	// --- Stage A runs, writes an artifact, returns a result envelope. ---
	produced := []byte("stage A computed this\n")
	ptrA, err := WriteArtifact(journal, "artifacts/stage-a/output.txt", produced, "text/plain")
	if err != nil {
		t.Fatalf("stage A WriteArtifact: %v", err)
	}
	resultA := ResultEnvelope{Status: ResultSuccess, Artifacts: []ArtifactPointer{ptrA}}

	// --- The runner builds stage B's invocation. It may pass POINTERS from A's
	// result, but not A's result body. Demonstrate that here: B's invocation is
	// assembled solely from A's artifact pointers. ---
	invB := InvocationEnvelope{
		TaskID: "stage-b", WorkflowID: "toy", RunID: "run-1", Gaggle: "g",
		Goal: "consume stage A's artifact", Workspace: filepath.Join(journal, "wt-b"),
		RepoRef: RepoRef{Provider: ProviderGitHub, Owner: "o", Name: "r"},
	}
	for i := range resultA.Artifacts {
		a := resultA.Artifacts[i]
		invB.ContextPointers = append(invB.ContextPointers, ContextPointer{Name: "from-a", Artifact: &a})
	}

	// --- Stage B runs: it resolves the pointer read-only, digest-verified. B has
	// no access to resultA — only invB.ContextPointers. ---
	if len(invB.ContextPointers) != 1 || invB.ContextPointers[0].Artifact == nil {
		t.Fatalf("stage B did not receive an artifact pointer")
	}
	got, err := invB.ContextPointers[0].Artifact.Resolve(journal)
	if err != nil {
		t.Fatalf("stage B Resolve: %v", err)
	}
	if string(got) != string(produced) {
		t.Errorf("stage B read %q, want %q (round-tripped by pointer + digest)", got, produced)
	}
}
