package gooberassets

import (
	"os"
	"path/filepath"
	"testing"
)

// Fingerprint is the bundle's identity, so a bundle that survives transport
// must fingerprint identically. That is what makes the claim-check transport
// VERIFIABLE: the receiver can prove it reconstructed the sender's bundle
// rather than trusting the channel that delivered it.
func TestWireRoundTripPreservesFingerprint(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "top.md"), []byte("top\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "nested", "deep.txt"), []byte("deep\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	original, err := Load(src)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	data, err := MarshalBundles(map[string]*Bundle{"g": original})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	back, err := UnmarshalBundles(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := back["g"]
	if got == nil {
		t.Fatal("bundle absent after round trip")
	}
	if got.Fingerprint() != original.Fingerprint() {
		t.Fatalf("fingerprint changed across the wire:\n  before %s\n  after  %s",
			original.Fingerprint(), got.Fingerprint())
	}
}

// A goober with no assets must travel as ABSENT, not as an empty bundle: an
// empty bundle materialises a directory the goober never had.
func TestWireNilBundleStaysNil(t *testing.T) {
	data, err := MarshalBundles(map[string]*Bundle{"g": nil})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	back, err := UnmarshalBundles(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back["g"] != nil {
		t.Fatal("a nil bundle must not become a non-nil empty one")
	}
}

// The reconstructed bundle must actually write the same files, not merely hash
// the same — Materialize is what the stage ultimately depends on.
func TestWireRoundTripMaterializesIdenticalTree(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	original, err := Load(src)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := MarshalBundles(map[string]*Bundle{"g": original})
	back, _ := UnmarshalBundles(data)

	ws := t.TempDir()
	if err := back["g"].Materialize(ws); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(ws, WorkspaceDir, "a.txt"))
	if err != nil {
		t.Fatalf("materialized tree missing the file: %v", err)
	}
	if string(got) != "alpha\n" {
		t.Fatalf("content = %q, want the original bytes", got)
	}
}
