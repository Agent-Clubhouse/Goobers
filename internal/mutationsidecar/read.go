// Package mutationsidecar bounds reads of the stage's provider receipt handoff.
package mutationsidecar

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/goobers/goobers/internal/platform/safeopen"
)

// FileName is the mutation sidecar's name inside a stage workspace. Every
// provider subcommand writes it in the workspace root regardless of the
// stage's declared resultFile, which is why the worktree layer excludes it
// from git by name (#5119).
const FileName = "mutations.jsonl"

// MaxBytes caps memory used by a single stage's receipt handoff. Oversized
// files are rejected, never truncated into an apparently complete receipt set.
const MaxBytes = 16 << 20

// MaxLines bounds the number of decoded facts or per-line diagnostics.
const MaxLines = 10000

// ReadFacts parses a sidecar for diagnostics without making malformed facts fatal.
func ReadFacts[T any](workspace string, validate func(line int, fact T) string) (facts []T, issues []string) {
	data, err := Read(workspace)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []string{fmt.Sprintf("read sidecar: %v", err)}
	}
	for i, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var fact T
		if err := json.Unmarshal(line, &fact); err != nil {
			issues = append(issues, fmt.Sprintf("line %d: %v", i+1, err))
			continue
		}
		if validate != nil {
			if issue := validate(i+1, fact); issue != "" {
				issues = append(issues, fmt.Sprintf("line %d: %s", i+1, issue))
				continue
			}
		}
		facts = append(facts, fact)
	}
	return facts, issues
}

// ReadHandoff returns a complete receipt set suitable for durable custody.
// Missing files are empty; legacy or conflicting receipt identities are errors.
// Unlike a diagnostic reader, this must never return an accepted prefix.
func ReadHandoff(workspace string) ([]Fact, error) {
	data, err := Read(workspace)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	facts, err := ParseRecoveryFacts(data)
	if err != nil {
		return nil, err
	}
	if _, err := missingRecoveryEvents(facts, nil, "handoff"); err != nil {
		return nil, err
	}
	return facts, nil
}

// Read reads a complete bounded regular-file handoff, refusing symlink leaves.
// Missing files retain os.IsNotExist compatibility for existing stage callers.
func Read(workspace string) (data []byte, err error) {
	dir, err := safeopen.Open(workspace)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := dir.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	f, err := safeopen.OpenAt(dir, FileName)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("mutation sidecar must be a regular file (mode %s)", info.Mode()&os.ModeType)
	}
	if info.Size() > MaxBytes {
		return nil, fmt.Errorf("mutation sidecar exceeds %d bytes", MaxBytes)
	}
	data, err = io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxBytes {
		return nil, fmt.Errorf("mutation sidecar exceeds %d bytes", MaxBytes)
	}
	lines := bytes.Count(data, []byte{'\n'})
	if len(data) > 0 && data[len(data)-1] != '\n' {
		lines++
	}
	if lines > MaxLines {
		return nil, fmt.Errorf("mutation sidecar exceeds %d lines", MaxLines)
	}
	return data, nil
}
