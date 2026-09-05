package v20

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// checkFrozenPatchLog is the DSL 2.0 lock's CI guard, mirroring v_current's
// DVL-9 (#869) mechanism: pure so it's directly testable against synthetic
// inputs (TestCheckFrozenPatchLogCatchesDrift below) as well as the real
// golden fixtures (TestFrozenGoldenChangeRequiresAcknowledgedPatch).
//
// This interpreter is locked as of v0.2.0 (#3279): DSL 2.0 is supported and
// final, and its compiled-digest goldens may only ever move via a reviewed,
// contract-preserving interpreter PATCH, never silently.
// TestGoldenCompiledSemanticDigests already fails on an ACCIDENTAL digest
// drift (compiled output no longer matches the committed digests.json); this
// closes the other half: a DELIBERATE digests.json edit must be accompanied
// by a human-authored PATCH_LOG.json entry, or CI stays red. digestsSha256
// pins digests.json's own committed bytes, so bumping it (the only way to
// make the guard pass again after a real digests.json change) forces the same
// PR to also touch PATCH_LOG.json's patches list — there is no path to a
// green frozen-golden change that skips recording why.
func checkFrozenPatchLog(digestsRaw, logRaw []byte) error {
	sum := sha256.Sum256(digestsRaw)
	gotSha := hex.EncodeToString(sum[:])

	var log frozenPatchLog
	if err := json.Unmarshal(logRaw, &log); err != nil {
		return fmt.Errorf("decode PATCH_LOG.json: %w", err)
	}

	if log.DigestsSha256 != gotSha {
		return fmt.Errorf(
			"digests.json changed (sha256 %s) without an acknowledged patch (PATCH_LOG.json pins %s) — "+
				"DSL 2.0 is locked as of v0.2.0 (#3279): a contract-preserving patch must bump "+
				"PATCH_LOG.json's digestsSha256 to %s and append a new patches[] entry describing the fix; "+
				"a feature or semantic change does not belong here at all — copy the interpreter forward instead",
			gotSha, log.DigestsSha256, gotSha)
	}
	if len(log.Patches) == 0 {
		return fmt.Errorf("PATCH_LOG.json has no patches[] entries — at least the initial freeze record is required")
	}
	last := log.Patches[len(log.Patches)-1]
	if last.Version != len(log.Patches) {
		return fmt.Errorf("last patch version = %d, want %d (patches[] must be a dense, ordered version log)", last.Version, len(log.Patches))
	}
	if strings.TrimSpace(last.BinaryVersion) == "" {
		return fmt.Errorf("patches[%d] has an empty binaryVersion", last.Version)
	}
	if strings.TrimSpace(last.Description) == "" {
		return fmt.Errorf("patches[%d] has an empty description — record what the patch fixed and why it's contract-preserving", last.Version)
	}
	return nil
}

func TestFrozenGoldenChangeRequiresAcknowledgedPatch(t *testing.T) {
	goldenDir := filepath.Join("testdata", "golden")

	digestsRaw, err := os.ReadFile(filepath.Join(goldenDir, "digests.json"))
	if err != nil {
		t.Fatalf("read digests.json: %v", err)
	}
	logRaw, err := os.ReadFile(filepath.Join(goldenDir, "PATCH_LOG.json"))
	if err != nil {
		t.Fatalf("read PATCH_LOG.json: %v", err)
	}
	if err := checkFrozenPatchLog(digestsRaw, logRaw); err != nil {
		t.Fatal(err)
	}
}

// TestCheckFrozenPatchLogCatchesDrift proves the guard actually fires: an
// unacknowledged digests.json edit (the sha256 lock stays stale) must fail,
// and every hygiene omission on a fresh entry (no description, no
// binaryVersion, a non-dense version number) must fail too. Otherwise this
// guard could silently rot into a no-op alongside the real fixtures.
func TestCheckFrozenPatchLogCatchesDrift(t *testing.T) {
	validLog := `{"digestsSha256":"%s","patches":[{"version":1,"binaryVersion":"v0.2.0","description":"initial freeze"}]}`
	digestsA := []byte(`{"a.yaml":{"machine":"m1","semantics":"s1"}}`)
	digestsB := []byte(`{"a.yaml":{"machine":"m2","semantics":"s2"}}`) // different content -> different sha256
	shaOf := func(b []byte) string {
		sum := sha256.Sum256(b)
		return hex.EncodeToString(sum[:])
	}

	t.Run("matching digest and complete log passes", func(t *testing.T) {
		log := fmt.Sprintf(validLog, shaOf(digestsA))
		if err := checkFrozenPatchLog(digestsA, []byte(log)); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("digests changed without bumping the log's pinned sha fails", func(t *testing.T) {
		log := fmt.Sprintf(validLog, shaOf(digestsA)) // still pins A's hash
		if err := checkFrozenPatchLog(digestsB, []byte(log)); err == nil {
			t.Fatal("want an error when digests.json content no longer matches PATCH_LOG.json's pinned sha256")
		}
	})

	t.Run("empty patches list fails", func(t *testing.T) {
		log := fmt.Sprintf(`{"digestsSha256":"%s","patches":[]}`, shaOf(digestsA))
		if err := checkFrozenPatchLog(digestsA, []byte(log)); err == nil {
			t.Fatal("want an error with no patches[] entries")
		}
	})

	t.Run("empty description on the latest entry fails", func(t *testing.T) {
		log := fmt.Sprintf(`{"digestsSha256":"%s","patches":[{"version":1,"binaryVersion":"v0.2.0","description":""}]}`, shaOf(digestsA))
		if err := checkFrozenPatchLog(digestsA, []byte(log)); err == nil {
			t.Fatal("want an error with an empty description")
		}
	})

	t.Run("empty binaryVersion on the latest entry fails", func(t *testing.T) {
		log := fmt.Sprintf(`{"digestsSha256":"%s","patches":[{"version":1,"binaryVersion":"","description":"fix"}]}`, shaOf(digestsA))
		if err := checkFrozenPatchLog(digestsA, []byte(log)); err == nil {
			t.Fatal("want an error with an empty binaryVersion")
		}
	})

	t.Run("non-dense version numbering fails", func(t *testing.T) {
		log := fmt.Sprintf(`{"digestsSha256":"%s","patches":[{"version":3,"binaryVersion":"v0.2.0","description":"fix"}]}`, shaOf(digestsA))
		if err := checkFrozenPatchLog(digestsA, []byte(log)); err == nil {
			t.Fatal("want an error when the latest version isn't len(patches)")
		}
	})
}

// frozenPatchLog is PATCH_LOG.json's shape: an append-only record of every
// reviewed patch applied to this locked interpreter, keyed to the exact
// digests.json content it produced.
type frozenPatchLog struct {
	// DigestsSha256 pins the digests.json bytes this log currently accounts
	// for — the guard's lock. It must be updated in the same change that
	// updates digests.json.
	DigestsSha256 string        `json:"digestsSha256"`
	Patches       []frozenPatch `json:"patches"`
}

// frozenPatch is one reviewed, contract-preserving interpreter patch.
type frozenPatch struct {
	// Version is this patch's 1-based ordinal in the log (dense, no gaps).
	Version int `json:"version"`
	// BinaryVersion is the goobers binary SemVer (REL-1/#431) this patch
	// shipped in, or "unreleased" for the initial freeze record.
	BinaryVersion string `json:"binaryVersion"`
	// Description explains what the patch fixed and why it preserves every
	// version's author-visible contract (§3.5's PATCH definition) rather
	// than being a feature or semantic change that belongs in a copied-
	// forward interpreter instead.
	Description string `json:"description"`
}
