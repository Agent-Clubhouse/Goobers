package intake

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
)

func migrationPrefixDigest(prefix []string) string {
	h := sha256.New()
	for _, migration := range prefix {
		h.Write([]byte(migration))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestMigrationPrefixIsAppendOnly(t *testing.T) {
	const wantDigest = "f0017acad67eeb5c0ee252220434bd8a3c458029e4ed589f5e8f9dec2edb252a"
	prefixLength := len(migrations) - 1
	if len(migrations) == 1 {
		// Pin the baseline immediately; after the first append it naturally
		// becomes the standard all-but-newest prefix.
		prefixLength = 1
	}
	if got := migrationPrefixDigest(migrations[:prefixLength]); got != wantDigest {
		t.Fatalf("migration prefix digest = %s, want %s\n"+
			"migrations must be append-only. If this commit only APPENDED a new\n"+
			"migration to the end of the list, update wantDigest to the value\n"+
			"above. If it did anything else to an existing entry, restore the\n"+
			"released migration.", got, wantDigest)
	}
}

func TestOpenRefusesANewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := store.db.Exec(`UPDATE schema_meta SET version = ?`, len(migrations)+5); err != nil {
		t.Fatalf("bump schema version: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := Open(path); err == nil {
		t.Fatal("opened a store whose schema is newer than this build supports")
	} else if !strings.Contains(err.Error(), "newer than this build") {
		t.Errorf("error = %v; want a clear newer-schema refusal", err)
	}
}
