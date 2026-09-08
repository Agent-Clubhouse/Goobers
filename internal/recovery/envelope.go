package recovery

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// WriteArchiveEnvelope writes a uint32 big-endian record length, the strict
// versioned JSON record, then exact archive bytes. No prefix is sent until
// CopyVerifiedArchive has verified private staged bytes. Authorization belongs
// to the calling delivery service, not to this binary framing helper.
func WriteArchiveEnvelope(ctx context.Context, path string, record Record, maxBytes int64, out io.Writer) error {
	if out == nil {
		return fmt.Errorf("recovery envelope requires a destination")
	}
	metadata, err := Encode(record)
	if err != nil {
		return err
	}
	prefix := make([]byte, 4+len(metadata))
	binary.BigEndian.PutUint32(prefix[:4], uint32(len(metadata)))
	copy(prefix[4:], metadata)
	return CopyVerifiedArchive(ctx, path, record, maxBytes, &archiveEnvelopeWriter{out: out, prefix: prefix})
}

type archiveEnvelopeWriter struct {
	out    io.Writer
	prefix []byte
}

func (w *archiveEnvelopeWriter) Write(data []byte) (int, error) {
	if w.prefix != nil {
		n, err := w.out.Write(w.prefix)
		if err != nil {
			return 0, err
		}
		if n != len(w.prefix) {
			return 0, io.ErrShortWrite
		}
		w.prefix = nil
	}
	return w.out.Write(data)
}

// ReceiveArchiveEnvelope writes into a caller-owned private staging directory.
// The caller must discard that directory on failure. A record is published only
// after exact size, digest and header verification; it is never inferred from
// received bytes. ImportSnapshotBundle must still verify Git objects and patch.
func ReceiveArchiveEnvelope(ctx context.Context, source io.Reader, directory string, maxBytes int64) (Record, error) {
	return receiveArchiveEnvelope(ctx, source, directory, maxBytes, nil)
}

// admit runs on validated metadata before any archive bytes are consumed or
// files published. It is an identity/policy check, never proof of blob integrity.
func receiveArchiveEnvelope(ctx context.Context, source io.Reader, directory string, maxBytes int64, admit func(Record) error) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	var length uint32
	if err := binary.Read(source, binary.BigEndian, &length); err != nil {
		return Record{}, err
	}
	if length == 0 || length > MaxRecordBytes {
		return Record{}, fmt.Errorf("invalid recovery envelope record length")
	}
	metadata := make([]byte, int(length))
	if _, err := io.ReadFull(source, metadata); err != nil {
		return Record{}, err
	}
	record, err := Decode(bytes.NewReader(metadata))
	if err != nil {
		return Record{}, err
	}
	if record.ArchiveBytes > maxBytes {
		return Record{}, fmt.Errorf("recovery envelope exceeds archive budget")
	}
	if admit != nil {
		if err := admit(record); err != nil {
			return Record{}, err
		}
	}
	path := filepath.Join(directory, BundleFileName)
	digest, err := publishArchive(ctx, path, record.ArchiveBytes, func(out io.Writer) error {
		_, err := io.Copy(recoveryContextWriter{ctx: ctx, destination: out}, source)
		return err
	})
	if err != nil {
		return Record{}, err
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != record.ArchiveBytes || digest != record.ArchiveDigest {
		return Record{}, fmt.Errorf("received recovery archive does not match its record")
	}
	if err := verifyBundleHeader(path, record); err != nil {
		return Record{}, err
	}
	if err := PublishRecord(filepath.Join(directory, RecordFileName), record); err != nil {
		return Record{}, err
	}
	return record, nil
}
