package recovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
)

// WriteSnapshotBundle streams a self-contained Git bundle, including the base
// history, so deleting the source repository need not destroy recovery objects.
// The caller must store this outside the repository being cleaned up, discard
// partial output on error, and durably publish it before acknowledging cleanup.
// maxBytes is mandatory: exceeding the budget fails closed rather than silently
// omitting objects. The returned digest covers exactly the emitted bundle.
func WriteSnapshotBundle(ctx context.Context, repository string, record Record, destination io.Writer, maxBytes int64) (string, error) {
	if destination == nil || maxBytes <= 0 {
		return "", fmt.Errorf("recovery bundle requires a destination and positive byte budget")
	}
	if err := PinCommit(ctx, repository, record); err != nil {
		return "", err
	}
	digest := sha256.New()
	output := &boundedBundleWriter{destination: io.MultiWriter(destination, digest), remaining: maxBytes}
	// Git requires a named ref. Verify its captured identity below, rather
	// than trusting that the ref stayed unchanged after PinCommit returned.
	if err := recoveryGit(ctx, repository, output, "bundle", "create", "--version=3", "-", record.Ref); err != nil {
		return "", fmt.Errorf("capture recovery bundle: %w", err)
	}
	if string(output.header) != expectedBundleHeader(record) {
		return "", fmt.Errorf("recovery bundle does not contain the expected self-contained snapshot")
	}
	return fmt.Sprintf("sha256:%x", digest.Sum(nil)), nil
}

func expectedBundleHeader(record Record) string {
	format := "sha1"
	if len(record.SnapshotSHA) == 64 {
		format = "sha256"
	}
	return fmt.Sprintf("# v3 git bundle\n@object-format=%s\n%s %s\n\n", format, record.SnapshotSHA, record.Ref)
}

type boundedBundleWriter struct {
	destination    io.Writer
	remaining      int64
	header         []byte
	headerComplete bool
}

func (w *boundedBundleWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.remaining {
		return 0, fmt.Errorf("recovery bundle exceeds byte budget")
	}
	for _, value := range data {
		if w.headerComplete {
			break
		}
		if len(w.header) >= 4096 {
			return 0, fmt.Errorf("recovery bundle header exceeds byte budget")
		}
		w.header = append(w.header, value)
		w.headerComplete = bytes.HasSuffix(w.header, []byte("\n\n"))
	}
	n, err := w.destination.Write(data)
	w.remaining -= int64(n)
	return n, err
}
