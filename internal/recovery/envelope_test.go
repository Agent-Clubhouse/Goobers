package recovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestRecoveryEnvelopeRoundTripAndMalformedDelivery(t *testing.T) {
	record := storageTestRecord()
	archive := []byte(expectedBundleHeader(record) + "binary\x00fixture")
	record.ArchiveBytes = int64(len(archive))
	record.ArchiveDigest = fmt.Sprintf("sha256:%x", sha256.Sum256(archive))
	path := filepath.Join(t.TempDir(), BundleFileName)
	if err := os.WriteFile(path, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	if err := WriteArchiveEnvelope(context.Background(), path, record, 4096, &wire); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"valid", "truncated-length", "oversized-record", "truncated-record", "truncated-archive", "tampered", "trailing", "budget"} {
		t.Run(mode, func(t *testing.T) {
			data := bytes.Clone(wire.Bytes())
			limit := int64(4096)
			switch mode {
			case "truncated-length":
				data = data[:3]
			case "oversized-record":
				binary.BigEndian.PutUint32(data[:4], MaxRecordBytes+1)
			case "truncated-record":
				data = data[:10]
			case "truncated-archive":
				data = data[:len(data)-1]
			case "tampered":
				data[len(data)-1] ^= 1
			case "trailing":
				data = append(data, 0)
			case "budget":
				limit = 1
			}
			directory := t.TempDir()
			got, err := ReceiveArchiveEnvelope(context.Background(), bytes.NewReader(data), directory, limit)
			if mode == "valid" {
				if err != nil || got != record {
					t.Fatalf("roundtrip: %+v %v", got, err)
				}
				stored, err := os.ReadFile(filepath.Join(directory, BundleFileName))
				if err != nil || !bytes.Equal(stored, archive) {
					t.Fatalf("binary archive changed: %v", err)
				}
			} else {
				if err == nil || got != (Record{}) {
					t.Fatalf("malformed envelope accepted: %+v %v", got, err)
				}
				if _, err := os.Stat(filepath.Join(directory, RecordFileName)); !os.IsNotExist(err) {
					t.Fatalf("malformed delivery published usable metadata: %v", err)
				}
			}
		})
	}
}

func TestRecoveryEnvelopeAdmissionPrecedesArchiveReadAndPublication(t *testing.T) {
	record := storageTestRecord()
	metadata, err := Encode(record)
	if err != nil {
		t.Fatal(err)
	}
	var prefix bytes.Buffer
	if err := binary.Write(&prefix, binary.BigEndian, uint32(len(metadata))); err != nil {
		t.Fatal(err)
	}
	prefix.Write(metadata)
	denied := errors.New("foreign run identity")
	body := &unreadRecoveryArchive{t: t}
	directory := t.TempDir()
	got, err := receiveArchiveEnvelope(context.Background(), io.MultiReader(&prefix, body), directory, record.ArchiveBytes, func(got Record) error {
		if got != record {
			t.Fatalf("admission metadata changed: %+v", got)
		}
		return denied
	})
	if !errors.Is(err, denied) || got != (Record{}) {
		t.Fatalf("admission refusal: %+v %v", got, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("refused archive wrote files: %v %v", entries, err)
	}
}

type unreadRecoveryArchive struct{ t *testing.T }

func (r *unreadRecoveryArchive) Read([]byte) (int, error) {
	r.t.Error("consumed archive bytes before identity admission")
	return 0, io.EOF
}

func TestRecoveryEnvelopeSendsNoMetadataForCorruptArchive(t *testing.T) {
	path := filepath.Join(t.TempDir(), BundleFileName)
	if err := os.WriteFile(path, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := WriteArchiveEnvelope(context.Background(), path, storageTestRecord(), 4096, &out); err == nil || out.Len() != 0 {
		t.Fatalf("unverified envelope prefix delivered: bytes=%d err=%v", out.Len(), err)
	}
}
