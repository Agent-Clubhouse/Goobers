package journal

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// The instance journal's live-compaction problem: (*InstanceLog).Compact
// atomically checkpoints events.jsonl WHILE the daemon (and any other
// independently-opened InstanceLog handle, or an unrelated reader like the
// portal/CLI) may still have it open. Rewriting the SAME path — even via the
// most tolerant Windows replace primitive available (both MoveFileEx and the
// dedicated ReplaceFile API were tried and both fail with a sharing violation
// against an ordinary open reader; see git history) — is a dead end on
// Windows: no rename/replace/delete API can act on a path some handle has
// open without FILE_SHARE_DELETE, and an arbitrary reader has no reason to
// request it.
//
// The fix is to never touch a path a reader might have open. Compaction
// writes to a NEW path — the next "generation" — and atomically advances a
// tiny pointer file that resolves "the current generation" from then on. A
// reader that already resolved the previous generation keeps its handle on a
// file nobody will ever touch again; Windows has nothing to object to. The
// pointer file itself is safe to replace with the ordinary durable-write
// primitive: nothing holds a lasting handle on it (resolveInstanceEventsPath
// opens, reads, and closes it in one call), so the brief window where
// MoveFileEx's FILE_SHARE_DELETE requirement could theoretically collide with
// a reader is negligible compared to events.jsonl, which stays open for the
// process's entire lifetime.
//
// Generation 0 keeps the legacy bare "events.jsonl" name and has no pointer
// file, so an instance directory that predates this scheme (or has never
// compacted) needs no migration: resolution falls back to generation 0 when
// the pointer is absent. Generation N>0 is "events.jsonl.gen-NNNNNN".

// instanceEventsFilename returns the on-disk filename for a generation.
// Generation 0 is the legacy bare name so pre-existing instance directories
// need no migration.
func instanceEventsFilename(gen int) string {
	if gen == 0 {
		return fileEvents
	}
	return fmt.Sprintf("%s.gen-%06d", fileEvents, gen)
}

// resolveInstanceEventsGeneration reads dir's pointer file, defaulting to
// generation 0 when it's absent (no compaction has happened yet, or dir
// predates this scheme).
func resolveInstanceEventsGeneration(dir string) (int, error) {
	data, err := os.ReadFile(filepath.Join(dir, fileEventsPointer))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("journal: read instance log pointer: %w", err)
	}
	gen, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
	if convErr != nil || gen < 0 {
		return 0, fmt.Errorf("journal: instance log pointer %q is not a valid generation", strings.TrimSpace(string(data)))
	}
	return gen, nil
}

// resolveInstanceEventsPath returns the current generation's file path for
// dir alongside the generation number itself.
func resolveInstanceEventsPath(dir string) (path string, gen int, err error) {
	gen, err = resolveInstanceEventsGeneration(dir)
	if err != nil {
		return "", 0, err
	}
	return filepath.Join(dir, instanceEventsFilename(gen)), gen, nil
}

// InstanceEventsPath returns the current on-disk path of the instance
// journal at dir — the exact file OpenInstanceLog/Append/Compact/
// ReadInstanceLog currently read and write, honoring the generation pointer
// (see the package-level comment above). Exported for callers that need the
// file's own mtime/size directly (e.g. a freshness/dead-man-switch health
// check) rather than its parsed events, so they don't hardcode the legacy
// bare "events.jsonl" name and silently go stale the first time a directory
// compacts past generation 0.
func InstanceEventsPath(dir string) (string, error) {
	path, _, err := resolveInstanceEventsPath(dir)
	return path, err
}

// advanceInstanceEventsPointer durably records nextGen as dir's current
// generation. Safe to replace on Windows even while readers hold the events
// file open: see the package-level comment above — nothing holds a lasting
// handle on the pointer file itself.
func advanceInstanceEventsPointer(dir string, nextGen int) error {
	return WriteFileAtomic(filepath.Join(dir, fileEventsPointer), []byte(strconv.Itoa(nextGen)), 0o644)
}

// parseInstanceEventsGeneration recovers the generation number from an
// instance journal filename, reporting ok=false for anything that is not a
// generation file (the pointer file, locks, unrelated names). Only the exact
// canonical spelling instanceEventsFilename produces is accepted, so a
// look-alike name is never mistaken for a reclaimable generation.
func parseInstanceEventsGeneration(name string) (int, bool) {
	if name == fileEvents {
		return 0, true
	}
	rest, ok := strings.CutPrefix(name, fileEvents+".gen-")
	if !ok {
		return 0, false
	}
	gen, err := strconv.Atoi(rest)
	if err != nil || gen <= 0 || instanceEventsFilename(gen) != name {
		return 0, false
	}
	return gen, true
}

// staleInstanceEventsGenerations lists, in ascending order, the generation
// numbers actually present in dir that are older than currentGen-1.
func staleInstanceEventsGenerations(dir string, currentGen int) ([]int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("journal: list instance log generations in %s: %w", dir, err)
	}
	var stale []int
	for _, entry := range entries {
		gen, ok := parseInstanceEventsGeneration(entry.Name())
		if !ok || gen > currentGen-2 {
			continue
		}
		stale = append(stale, gen)
	}
	sort.Ints(stale)
	return stale, nil
}

// cleanupStaleInstanceEventsGenerations removes EVERY generation older than
// currentGen-1 that is still on disk, returning how many it reclaimed.
// Keeping the immediately-previous generation (currentGen-1) alongside
// current covers a reader that resolved the pointer a moment before this
// compaction advanced it: that reader might still be about to open what was,
// at the moment it read the pointer, current. No plausible reader holds a
// handle across two full compaction cycles, so anything older is safe to
// remove.
//
// Sweeping the whole directory rather than only currentGen-2 matters because
// a single failed removal used to strand that generation forever: the next
// compaction looked one generation further along and never came back for it.
// Removal failures are aggregated and returned so the caller can surface an
// actionable diagnostic, but they are deliberately NOT fatal to compaction —
// the pointer file is the only thing readers and writers trust, so a
// compaction that recorded new data must still be reported as successful.
func cleanupStaleInstanceEventsGenerations(dir string, currentGen int) (int, error) {
	if currentGen < 2 {
		return 0, nil // no generation has fallen out of the keep window yet
	}
	stale, err := staleInstanceEventsGenerations(dir, currentGen)
	if err != nil {
		return 0, err
	}
	removed := 0
	var failures []string
	for _, gen := range stale {
		name := instanceEventsFilename(gen)
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			failures = append(failures, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		removed++
	}
	if len(failures) > 0 {
		return removed, fmt.Errorf(
			"journal: could not remove %d stale instance log generation(s) in %s (compaction itself succeeded; delete them manually or check for a process holding them open): %s",
			len(failures), dir, strings.Join(failures, "; "))
	}
	return removed, nil
}
