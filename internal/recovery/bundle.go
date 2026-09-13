package recovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"strings"
)

// Archive formats recorded on Record.ArchiveFormat so a later restore knows
// which precondition applies.
const (
	archiveFormatFull  = "full"
	archiveFormatDelta = "delta"
)

// WriteSnapshotBundle streams a Git bundle capturing record.Ref. When the
// base commit is provably still reachable from its own tracked base ref
// (baseProvablyReachable), the bundle excludes that base's history entirely
// (format "delta"), so its size scales with the captured diff rather than
// with the base repository's history. Otherwise it streams the original
// self-contained bundle, including the base history, so deleting the source
// repository need not destroy recovery objects (format "full"). The caller
// must store this outside the repository being cleaned up, discard partial
// output on error, and durably publish it before acknowledging cleanup.
// maxBytes is mandatory: exceeding the budget fails closed rather than
// silently omitting objects. The returned digest covers exactly the emitted
// bundle bytes; format must be recorded so restore applies the right
// precondition.
func WriteSnapshotBundle(ctx context.Context, repository string, record Record, destination io.Writer, maxBytes int64) (digest, format string, err error) {
	if destination == nil || maxBytes <= 0 {
		return "", "", fmt.Errorf("recovery bundle requires a destination and positive byte budget")
	}
	if err := PinCommit(ctx, repository, record); err != nil {
		return "", "", err
	}
	format = archiveFormatFull
	if baseProvablyReachable(ctx, repository, record) {
		format = archiveFormatDelta
	}
	args := []string{"bundle", "create", "--version=3", "-", record.Ref}
	if format == archiveFormatDelta {
		args = append(args, "--not", record.BaseSHA)
	}
	sum := sha256.New()
	// Git requires a named ref. Verify its captured identity below, rather
	// than trusting that the ref stayed unchanged after PinCommit returned.
	output := &boundedBundleWriter{destination: io.MultiWriter(destination, sum), remaining: maxBytes}
	if err := recoveryGit(ctx, repository, output, args...); err != nil {
		return "", "", fmt.Errorf("capture recovery bundle: %w", err)
	}
	if err := verifyBundleHeaderBytes(output.header, record, format); err != nil {
		return "", "", err
	}
	return fmt.Sprintf("sha256:%x", sum.Sum(nil)), format, nil
}

// baseProvablyReachable proves the base commit remains reachable from its own
// tracked base ref independent of this run's workspace, so excluding its
// history from the bundle cannot create a knowingly unrestorable delta
// (maintainer decision, #4862). A record with no BaseRef (pre-#4823) or a
// base ref that has since been rewritten past the base falls back to "full".
func baseProvablyReachable(ctx context.Context, repository string, record Record) bool {
	if record.BaseRef == "" {
		return false
	}
	return recoveryGit(ctx, repository, io.Discard, "merge-base", "--is-ancestor", record.BaseSHA, record.BaseRef) == nil
}

func expectedBundleHeader(record Record) string {
	format := "sha1"
	if len(record.SnapshotSHA) == 64 {
		format = "sha256"
	}
	return fmt.Sprintf("# v3 git bundle\n@object-format=%s\n%s %s\n\n", format, record.SnapshotSHA, record.Ref)
}

// verifyBundleHeaderBytes confirms a captured bundle header, including its
// trailing blank line, matches exactly the declared format: a full bundle
// names only record.Ref, a delta bundle also declares record.BaseSHA as its
// sole prerequisite (the object it requires the unbundling repository to
// already hold).
func verifyBundleHeaderBytes(header []byte, record Record, format string) error {
	switch format {
	case archiveFormatFull:
		if string(header) != expectedBundleHeader(record) {
			return fmt.Errorf("recovery bundle does not contain the expected self-contained snapshot")
		}
	case archiveFormatDelta:
		trimmed, ok := strings.CutSuffix(string(header), "\n\n")
		lines := strings.Split(trimmed, "\n")
		objectFormat := "sha1"
		if len(record.SnapshotSHA) == 64 {
			objectFormat = "sha256"
		}
		if !ok || len(lines) != 4 || lines[0] != "# v3 git bundle" || lines[1] != "@object-format="+objectFormat ||
			!strings.HasPrefix(lines[2], "-"+record.BaseSHA) || lines[3] != record.SnapshotSHA+" "+record.Ref {
			return fmt.Errorf("recovery bundle does not contain the expected delta snapshot")
		}
	default:
		return fmt.Errorf("unknown recovery bundle format %q", format)
	}
	return nil
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
