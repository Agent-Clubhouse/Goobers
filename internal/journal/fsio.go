package journal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/internal/platform/durability"
)

// writeContent scrubs data, content-addresses it, and writes it to dir/relPath,
// returning a Ref whose Digest commits to the scrubbed bytes.
func writeContent(dir, relPath string, data []byte, scrubber Scrubber) (Ref, error) {
	scrubbed := scrubber.Scrub(data)
	return writeContentScrubbed(dir, relPath, scrubbed, Digest(scrubbed))
}

// writeContentScrubbed writes already-scrubbed bytes to dir/relPath atomically
// (temp + fsync + rename) and returns its Ref. If the target already exists with
// the same content address (artifact dedup), the write is skipped.
func writeContentScrubbed(dir, relPath string, scrubbed []byte, digest string) (Ref, error) {
	full := filepath.Join(dir, relPath)
	ref := Ref{Path: relPath, Digest: digest, Size: int64(len(scrubbed))}
	// Skip the write only when the blob already at rest genuinely has this
	// content address. A size match alone is NOT sufficient: a same-size blob
	// with different content — e.g. a redaction that replaces a secret with the
	// equal-length placeholder — would otherwise be treated as already stored and
	// the leaked bytes would survive on disk (SEC-041). Verify the digest before
	// skipping.
	if fi, err := os.Stat(full); err == nil && fi.Size() == ref.Size {
		if existing, rerr := os.ReadFile(full); rerr == nil && Digest(existing) == digest {
			return ref, nil // already stored (content-addressed dedup)
		}
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return Ref{}, err
	}
	if err := writeFileAtomic(full, scrubbed, 0o644); err != nil {
		return Ref{}, err
	}
	return ref, nil
}

// writeRunYAML serializes the pinned identity to run.yaml atomically.
func writeRunYAML(dir string, id RunIdentity) error {
	// sigs.k8s.io/yaml marshals via JSON tags, so run.yaml mirrors the JSON
	// contract exactly.
	b, err := yaml.Marshal(id)
	if err != nil {
		return fmt.Errorf("journal: marshal run.yaml: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(dir, fileRunYAML), b, 0o644); err != nil {
		return fmt.Errorf("journal: write run.yaml: %w", err)
	}
	return nil
}

// writeStateAtomic writes state.json via a temp file and rename, so a reader (or
// a crash) never observes a half-written checkpoint.
func writeStateAtomic(dir string, st State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("journal: marshal state.json: %w", err)
	}
	b = append(b, '\n')
	tmp := filepath.Join(dir, fileStateTemp)
	if err := writeFileSynced(tmp, b, 0o644); err != nil {
		return fmt.Errorf("journal: write state temp: %w", err)
	}
	if err := durability.ReplaceFile(tmp, filepath.Join(dir, fileState)); err != nil {
		return fmt.Errorf("journal: commit state.json: %w", err)
	}
	return fsyncDir(dir)
}

// WriteFileAtomic writes data to path via a sibling temp file, fsync, and
// rename, so a reader never observes a half-written file and the write
// survives a crash immediately after — the same durability primitive
// state.json and run.yaml use, exported for other instance-state durable files
// (e.g. the scheduler's claim ledger) that want the identical guarantee without
// duplicating it.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	return writeFileAtomic(path, data, perm)
}

// writeFileAtomic writes data to path via a sibling temp file, fsync, and rename.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := writeFileSynced(tmp, data, perm); err != nil {
		return err
	}
	if err := durability.ReplaceFile(tmp, path); err != nil {
		return err
	}
	return fsyncDir(filepath.Dir(path))
}

// writeFileSynced writes data to path and fsyncs the file before returning.
func writeFileSynced(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := syncFile(f); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// fsyncDir fsyncs a directory so a rename into it is durable across a crash.
func fsyncDir(dir string) error {
	if fsyncDisabled() {
		return nil
	}
	if err := durability.SyncDir(dir); err != nil {
		return fmt.Errorf("journal: fsync dir %s: %w", dir, err)
	}
	return nil
}

// sortedKeys returns the keys of m in deterministic order, so input snapshots
// are pinned to run.yaml in a stable sequence.
func sortedKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
