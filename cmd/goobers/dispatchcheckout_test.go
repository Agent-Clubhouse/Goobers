package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/dispatcher"
)

// `git clone <url> .` refuses a non-empty destination, so NOTHING the checkout
// creates for its own use may land in the workspace.
//
// MEASURED, from inside a stage pod, when the askpass helper was written there:
//
//	fatal: destination path '.' already exists and is not an empty directory
//
// The clone failed before contacting the remote at all, which made a working
// credential look like a broken one.
func TestCheckoutAuthMaterialStaysOutOfTheWorkspace(t *testing.T) {
	ws := t.TempDir()
	creds := []dispatcher.MintedCredential{{Capability: "repo:push", Value: "t0ken"}}

	if _, err := checkoutGitAuthEnv(ws, creds); err != nil {
		t.Fatalf("build git auth env: %v", err)
	}

	entries, err := os.ReadDir(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("workspace is not empty after preparing git auth: %v — git clone will refuse it", names)
	}
}

// No repo-shaped capability means an anonymous clone, NOT a substituted
// credential: handing a stage's unrelated token to git would be silent scope
// widening.
func TestCheckoutRefusesToSubstituteAnUnrelatedCredential(t *testing.T) {
	ws := t.TempDir()
	creds := []dispatcher.MintedCredential{{Capability: "github:issues:read", Value: "issues-only"}}

	env, err := checkoutGitAuthEnv(ws, creds)
	if err != nil {
		t.Fatalf("build git auth env: %v", err)
	}
	for _, kv := range env {
		if filepath.Base(kv) == "issues-only" || kv == "GOOBERS_GIT_TOKEN=issues-only" {
			t.Fatal("an unrelated credential was handed to git")
		}
	}
	if len(env) != 0 {
		t.Fatalf("expected no git auth env for a non-repo capability, got %v", env)
	}
}
