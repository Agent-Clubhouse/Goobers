package recovery

import (
	"strings"
	"testing"
	"time"
)

func TestRestorationIdentityBindsSourceNotRetentionWindow(t *testing.T) {
	record := storageTestRecord()
	message := restorationMessage(record)
	if !matchesRestorationMessage(record, message+"\n") {
		t.Fatal("own source identity refused")
	}
	for name, mutate := range map[string]func(*Record){
		"run":        func(r *Record) { r.RunID += "-other" },
		"repository": func(r *Record) { r.RepositoryKey += "-other" },
		"base":       func(r *Record) { r.BaseSHA = strings.Repeat("e", 40) },
		"snapshot":   func(r *Record) { r.SnapshotSHA = strings.Repeat("f", 40) },
		"patch":      func(r *Record) { r.PatchDigest = "sha256:" + strings.Repeat("d", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := record
			mutate(&changed)
			if matchesRestorationMessage(changed, message) {
				t.Fatal("different source accepted")
			}
		})
	}
	record.RetainUntil = record.RetainUntil.Add(time.Hour)
	record.ArchiveDigest = "sha256:" + strings.Repeat("9", 64)
	record.ArchiveBytes++
	if restorationMessage(record) != message {
		t.Fatal("retention renewal or archive repackaging changed patch identity")
	}
}

func TestRestorationLegacyMessageRequiresExactOwner(t *testing.T) {
	record := storageTestRecord()
	legacy := "Restore retained implementation for " + record.RunID
	if !matchesRestorationMessage(record, legacy) {
		t.Fatal("legacy preparation discovery refused")
	}
	for _, message := range []string{legacy + "-other", legacy + "\n\nGoobers-Recovery-Source: forged", "prefix " + legacy} {
		if matchesRestorationMessage(record, message) {
			t.Fatalf("unbound message accepted: %q", message)
		}
	}
}
