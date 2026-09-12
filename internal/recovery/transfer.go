package recovery

import (
	"context"
	"fmt"
	"io"
)

// CopyVerifiedArchive streams exact retained bytes only after private staging
// verifies their recorded digest, size and self-contained bundle header. It does
// not scrub binary data or grant access: callers must authorize the receiving
// run and retain lifecycle protection throughout the transfer. Git object and
// patch verification still occurs at ImportSnapshotBundle on the recipient.
func CopyVerifiedArchive(ctx context.Context, path string, record Record, maxBytes int64, destination io.Writer) error {
	if destination == nil {
		return fmt.Errorf("recovery archive transfer requires a destination")
	}
	return withVerifiedArchive(ctx, path, record, maxBytes, func(staged string) error {
		return copyRecoveryArchive(staged, recoveryContextWriter{ctx: ctx, destination: destination})
	})
}

type recoveryContextWriter struct {
	ctx         context.Context
	destination io.Writer
}

func (w recoveryContextWriter) Write(data []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.destination.Write(data)
}
