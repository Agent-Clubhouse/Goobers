package recovery

import (
	"bytes"
	"context"
	"fmt"
	"strings"
)

// maxTouchedPathBytes bounds the path listing a single snapshot may produce.
// It is a worthlessness check, not a restore path: a snapshot whose listing
// does not fit is certainly not a handful of bookkeeping files, and refusing
// it keeps the decision fail-closed.
const maxTouchedPathBytes = 1 << 20

// SnapshotTouchedPaths lists the repository-relative paths the retained
// snapshot changes against its base, read from repository's own object
// database. It never falls back to a guess: if the objects are unavailable —
// a mirror that never received the snapshot ref, a pruned base — it returns
// an error, so a caller deciding whether the snapshot is worth keeping must
// treat it as content-bearing rather than as empty.
func SnapshotTouchedPaths(ctx context.Context, repository string, record Record) ([]string, error) {
	if err := record.Validate(); err != nil {
		return nil, err
	}
	var output touchedPathOutput
	// core.quotePath=false with -z gives raw bytes, so a non-ASCII path is
	// compared as itself rather than as Git's escaped display form.
	err := recoveryGit(ctx, repository, &output,
		"-c", "core.quotePath=false", "diff", "--name-only", "-z",
		"--no-renames", "--no-ext-diff", "--no-textconv", "--no-color",
		"--ignore-submodules=none", record.BaseSHA, record.SnapshotSHA, "--")
	if err != nil {
		return nil, fmt.Errorf("list recovery snapshot paths: %w", err)
	}
	var paths []string
	for _, path := range strings.Split(output.String(), "\x00") {
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths, nil
}

type touchedPathOutput struct{ bytes.Buffer }

func (w *touchedPathOutput) Write(data []byte) (int, error) {
	if w.Len()+len(data) > maxTouchedPathBytes {
		return 0, fmt.Errorf("recovery snapshot path listing exceeds byte budget")
	}
	return w.Buffer.Write(data)
}
