package recovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestCopyVerifiedArchiveRefusesInvalidBytesBeforeDelivery(t *testing.T) {
	for _, mode := range []string{"valid", "digest", "size", "header", "budget", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			record := storageTestRecord()
			data := []byte(expectedBundleHeader(record) + "binary\x00fixture")
			if mode == "header" {
				data[0] = '!'
			}
			record.ArchiveBytes = int64(len(data))
			record.ArchiveDigest = fmt.Sprintf("sha256:%x", sha256.Sum256(data))
			if mode == "digest" {
				data[len(data)-1] ^= 1
			}
			if mode == "size" {
				record.ArchiveBytes++
			}
			limit := int64(4096)
			if mode == "budget" {
				limit = 1
			}
			path := filepath.Join(t.TempDir(), "snapshot.bundle")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancelled" {
				cancel()
			}
			var out bytes.Buffer
			err := CopyVerifiedArchive(ctx, path, record, limit, &out)
			if mode == "valid" {
				if err != nil || !bytes.Equal(out.Bytes(), data) {
					t.Fatalf("exact bytes not delivered: %v", err)
				}
			} else if err == nil || out.Len() != 0 {
				t.Fatalf("unverified bytes delivered: err=%v bytes=%d", err, out.Len())
			}
		})
	}
}
