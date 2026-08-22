package providerstage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/capability"
)

// This file is the manifest's drift guard. The subcommand→capability-
// requirement table above is consumed by exact-membership admission
// (internal/workflow v_current/v_next compile.go), so ANY edit — narrowing,
// broadening, a new conditional flag, a flipped claim-mutation bit — changes
// what every already-shipped config must declare, with no DSL-version linkage
// to soften it. TBH-1 (#2386, commit ed11ae81) proved the class: narrowing
// backlog-dedupe from github:issues:write to github:issues:read broke every
// existing config, and nothing in CI could have flagged the edit as
// contract-affecting. testdata/manifest_snapshot.json is a committed, byte-
// exact rendering of everything Command exposes; changing it requires an
// acknowledged entry in testdata/MANIFEST_PATCH_LOG.json — the same
// sha256-pinned mechanism as v_current's frozen-golden PATCH_LOG (DVL-9/#869,
// internal/workflow/v_current/frozen_patch_test.go).

// snapshotCapabilityUse mirrors CapabilityUse field-for-field, including the
// unexported conditional machinery, so no requirement surface can change
// without moving the rendered bytes.
type snapshotCapabilityUse struct {
	Capability  capability.Capability `json:"capability"`
	Consequence string                `json:"consequence"`
	Optional    bool                  `json:"optional,omitempty"`
	Flag        string                `json:"flag,omitempty"`
	FlagValue   string                `json:"flagValue,omitempty"`
	AnyFlags    []string              `json:"anyFlags,omitempty"`
	UnlessFlags []string              `json:"unlessFlags,omitempty"`
}

// snapshotCommand mirrors Command plus its manifest key.
type snapshotCommand struct {
	Command            string                  `json:"command"`
	ResultFile         string                  `json:"resultFile,omitempty"`
	MutatesClaimLedger bool                    `json:"mutatesClaimLedger,omitempty"`
	ClaimMutationFlags []string                `json:"claimMutationFlags,omitempty"`
	Capabilities       []snapshotCapabilityUse `json:"capabilities"`
}

// renderManifestSnapshot serializes table deterministically (commands sorted
// by name, capabilities in declaration order) so a byte comparison against
// the committed snapshot is stable.
func renderManifestSnapshot(table map[string]Command) ([]byte, error) {
	names := make([]string, 0, len(table))
	for name := range table {
		names = append(names, name)
	}
	slices.Sort(names)
	entries := make([]snapshotCommand, 0, len(names))
	for _, name := range names {
		entry := table[name]
		uses := make([]snapshotCapabilityUse, 0, len(entry.Capabilities))
		for _, use := range entry.Capabilities {
			uses = append(uses, snapshotCapabilityUse{
				Capability:  use.Capability,
				Consequence: use.Consequence,
				Optional:    use.optional,
				Flag:        use.flag,
				FlagValue:   use.flagValue,
				AnyFlags:    use.anyFlags,
				UnlessFlags: use.unlessFlags,
			})
		}
		entries = append(entries, snapshotCommand{
			Command:            name,
			ResultFile:         entry.ResultFile,
			MutatesClaimLedger: entry.mutatesClaimLedger,
			ClaimMutationFlags: entry.claimMutationFlags,
			Capabilities:       uses,
		})
	}
	raw, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal manifest snapshot: %w", err)
	}
	return append(raw, '\n'), nil
}

// checkManifestPatchLog mirrors v_current's checkFrozenPatchLog (DVL-9/#869):
// MANIFEST_PATCH_LOG.json pins the exact committed snapshot bytes by sha256,
// so making CI green again after a snapshot change forces the same PR to
// append a changes[] entry recording what moved and why existing configs
// survive it — there is no path to a green manifest change that skips the
// acknowledgment.
func checkManifestPatchLog(snapshotRaw, logRaw []byte) error {
	sum := sha256.Sum256(snapshotRaw)
	gotSha := hex.EncodeToString(sum[:])

	var log manifestPatchLog
	if err := json.Unmarshal(logRaw, &log); err != nil {
		return fmt.Errorf("decode MANIFEST_PATCH_LOG.json: %w", err)
	}

	if log.SnapshotSha256 != gotSha {
		return fmt.Errorf(
			"manifest_snapshot.json changed (sha256 %s) without an acknowledged change (MANIFEST_PATCH_LOG.json pins %s) — "+
				"the subcommand→capability table is a shipped contract (#2386 broke every existing config by editing it silently): "+
				"bump MANIFEST_PATCH_LOG.json's snapshotSha256 to %s and append a changes[] entry stating what moved and why "+
				"existing configs keep compiling",
			gotSha, log.SnapshotSha256, gotSha)
	}
	if len(log.Changes) == 0 {
		return fmt.Errorf("MANIFEST_PATCH_LOG.json has no changes[] entries — at least the initial snapshot record is required")
	}
	last := log.Changes[len(log.Changes)-1]
	if last.Version != len(log.Changes) {
		return fmt.Errorf("last change version = %d, want %d (changes[] must be a dense, ordered version log)", last.Version, len(log.Changes))
	}
	if strings.TrimSpace(last.Description) == "" {
		return fmt.Errorf("changes[%d] has an empty description — record what changed and why it does not break existing configs", last.Version)
	}
	return nil
}

// manifestPatchLog is MANIFEST_PATCH_LOG.json's shape: an append-only record
// of every reviewed capability-requirement change, keyed to the exact
// manifest_snapshot.json content it produced.
type manifestPatchLog struct {
	// SnapshotSha256 pins the manifest_snapshot.json bytes this log currently
	// accounts for — the guard's lock. It must be updated in the same change
	// that updates the snapshot.
	SnapshotSha256 string           `json:"snapshotSha256"`
	Changes        []manifestChange `json:"changes"`
}

// manifestChange is one reviewed change to the requirement table.
type manifestChange struct {
	// Version is this change's 1-based ordinal in the log (dense, no gaps).
	Version int `json:"version"`
	// Description explains what changed and why existing configs keep
	// compiling — e.g. a purely additive command, or a narrowing paired with
	// admission-side subsumption (internal/capability.Subsumes).
	Description string `json:"description"`
}

func TestManifestSnapshotMatchesLiveTable(t *testing.T) {
	got, err := renderManifestSnapshot(commands)
	if err != nil {
		t.Fatalf("render manifest snapshot: %v", err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "manifest_snapshot.json"))
	if err != nil {
		t.Fatalf("read committed snapshot: %v", err)
	}
	if !bytes.Equal(got, want) {
		rejected, err := os.CreateTemp("", "manifest_snapshot-*.json")
		if err == nil {
			_, _ = rejected.Write(got)
			_ = rejected.Close()
		}
		t.Fatalf("manifest.go no longer matches testdata/manifest_snapshot.json — the capability-requirement table is a shipped contract (#2386): "+
			"if the edit is intended, replace the snapshot with the freshly rendered copy at %s AND acknowledge it in testdata/MANIFEST_PATCH_LOG.json (new snapshotSha256 + a changes[] entry)", rejected.Name())
	}
}

func TestManifestSnapshotChangeRequiresAcknowledgedEntry(t *testing.T) {
	snapshotRaw, err := os.ReadFile(filepath.Join("testdata", "manifest_snapshot.json"))
	if err != nil {
		t.Fatalf("read manifest_snapshot.json: %v", err)
	}
	logRaw, err := os.ReadFile(filepath.Join("testdata", "MANIFEST_PATCH_LOG.json"))
	if err != nil {
		t.Fatalf("read MANIFEST_PATCH_LOG.json: %v", err)
	}
	if err := checkManifestPatchLog(snapshotRaw, logRaw); err != nil {
		t.Fatal(err)
	}
}

// TestRenderManifestSnapshotReflectsEveryExposedSurface proves the byte guard
// actually fires: for each field Command/CapabilityUse exposes, a table with
// that field edited must render differently from the live table. Without
// this, a snapshot field could silently fall out of the rendering (say, a
// dropped struct tag) and edits to it would sail past the byte comparison.
func TestRenderManifestSnapshotReflectsEveryExposedSurface(t *testing.T) {
	baseline, err := renderManifestSnapshot(commands)
	if err != nil {
		t.Fatalf("render baseline snapshot: %v", err)
	}

	clone := func() map[string]Command {
		out := make(map[string]Command, len(commands))
		for name, entry := range commands {
			entry.Capabilities = append([]CapabilityUse(nil), entry.Capabilities...)
			entry.claimMutationFlags = append([]string(nil), entry.claimMutationFlags...)
			out[name] = entry
		}
		return out
	}

	mutations := []struct {
		name   string
		mutate func(table map[string]Command)
	}{
		{"command removed", func(table map[string]Command) {
			delete(table, "backlog-dedupe")
		}},
		{"command added", func(table map[string]Command) {
			table["invented-command"] = Command{Capabilities: []CapabilityUse{required(capability.RepoRead, "test-only")}}
		}},
		// The exact #2386 shape, inverted: swapping a required capability.
		{"required capability changed", func(table map[string]Command) {
			entry := table["backlog-dedupe"]
			entry.Capabilities[0].Capability = capability.GitHubIssuesWrite
			table["backlog-dedupe"] = entry
		}},
		{"consequence reworded", func(table map[string]Command) {
			entry := table["backlog-dedupe"]
			entry.Capabilities[0].Consequence = "reworded"
			table["backlog-dedupe"] = entry
		}},
		{"optional flipped to required", func(table map[string]Command) {
			entry := table["backlog-query"]
			for i, use := range entry.Capabilities {
				if use.optional {
					entry.Capabilities[i].optional = false
				}
			}
			table["backlog-query"] = entry
		}},
		{"conditional flag renamed", func(table map[string]Command) {
			entry := table["telemetry-query"]
			entry.Capabilities[1].flag = "--output"
			table["telemetry-query"] = entry
		}},
		{"conditional flag value changed", func(table map[string]Command) {
			entry := table["telemetry-query"]
			entry.Capabilities[1].flagValue = "other-format"
			table["telemetry-query"] = entry
		}},
		{"anyFlags list changed", func(table map[string]Command) {
			entry := table["backlog-health"]
			entry.Capabilities[0].anyFlags = []string{"feedback", "extra"}
			table["backlog-health"] = entry
		}},
		{"unlessFlags list changed", func(table map[string]Command) {
			entry := table["respond-to-findings"]
			entry.Capabilities[0].unlessFlags = []string{"dry-run"}
			table["respond-to-findings"] = entry
		}},
		{"result file changed", func(table map[string]Command) {
			entry := table["backlog-dedupe"]
			entry.ResultFile = "other.json"
			table["backlog-dedupe"] = entry
		}},
		{"mutatesClaimLedger flipped", func(table map[string]Command) {
			entry := table["select-source"]
			entry.mutatesClaimLedger = false
			table["select-source"] = entry
		}},
		{"claimMutationFlags changed", func(table map[string]Command) {
			entry := table["backlog-query"]
			entry.claimMutationFlags = []string{"claim"}
			table["backlog-query"] = entry
		}},
	}

	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			table := clone()
			mutation.mutate(table)
			got, err := renderManifestSnapshot(table)
			if err != nil {
				t.Fatalf("render mutated snapshot: %v", err)
			}
			if bytes.Equal(got, baseline) {
				t.Fatal("mutated table rendered identically to the live table — this surface is invisible to the snapshot guard")
			}
		})
	}

	// The comparisons above are only meaningful if rendering is
	// deterministic; a map-order-dependent rendering would make the byte
	// guard flaky instead of protective.
	again, err := renderManifestSnapshot(commands)
	if err != nil {
		t.Fatalf("re-render snapshot: %v", err)
	}
	if !bytes.Equal(again, baseline) {
		t.Fatal("renderManifestSnapshot is not deterministic across calls")
	}
}

// TestCheckManifestPatchLogCatchesDrift proves the acknowledgment guard
// fires, mirroring v_current's TestCheckFrozenPatchLogCatchesDrift: an
// unacknowledged snapshot edit (stale pinned sha) must fail, and every
// hygiene omission on a fresh entry must fail too, or the guard could rot
// into a no-op alongside the real fixtures.
func TestCheckManifestPatchLogCatchesDrift(t *testing.T) {
	validLog := `{"snapshotSha256":"%s","changes":[{"version":1,"description":"initial snapshot"}]}`
	snapshotA := []byte(`[{"command":"a","capabilities":[]}]`)
	snapshotB := []byte(`[{"command":"b","capabilities":[]}]`) // different content -> different sha256
	shaOf := func(b []byte) string {
		sum := sha256.Sum256(b)
		return hex.EncodeToString(sum[:])
	}

	t.Run("matching sha and complete log passes", func(t *testing.T) {
		log := fmt.Sprintf(validLog, shaOf(snapshotA))
		if err := checkManifestPatchLog(snapshotA, []byte(log)); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("snapshot changed without bumping the pinned sha fails", func(t *testing.T) {
		log := fmt.Sprintf(validLog, shaOf(snapshotA)) // still pins A's hash
		if err := checkManifestPatchLog(snapshotB, []byte(log)); err == nil {
			t.Fatal("want an error when manifest_snapshot.json no longer matches MANIFEST_PATCH_LOG.json's pinned sha256")
		}
	})

	t.Run("empty changes list fails", func(t *testing.T) {
		log := fmt.Sprintf(`{"snapshotSha256":"%s","changes":[]}`, shaOf(snapshotA))
		if err := checkManifestPatchLog(snapshotA, []byte(log)); err == nil {
			t.Fatal("want an error with no changes[] entries")
		}
	})

	t.Run("empty description on the latest entry fails", func(t *testing.T) {
		log := fmt.Sprintf(`{"snapshotSha256":"%s","changes":[{"version":1,"description":"  "}]}`, shaOf(snapshotA))
		if err := checkManifestPatchLog(snapshotA, []byte(log)); err == nil {
			t.Fatal("want an error with an empty description")
		}
	})

	t.Run("non-dense version numbering fails", func(t *testing.T) {
		log := fmt.Sprintf(`{"snapshotSha256":"%s","changes":[{"version":3,"description":"fix"}]}`, shaOf(snapshotA))
		if err := checkManifestPatchLog(snapshotA, []byte(log)); err == nil {
			t.Fatal("want an error when the latest version isn't len(changes)")
		}
	})

	t.Run("malformed log fails", func(t *testing.T) {
		if err := checkManifestPatchLog(snapshotA, []byte(`{`)); err == nil {
			t.Fatal("want an error on undecodable MANIFEST_PATCH_LOG.json")
		}
	})
}
