package recovery

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// restorationMessage carries a stable identity for the source patch in the
// receiving Git history, including when the archive was downloaded to temporary
// storage. It is a discovery hint, never merge or content proof: callers must
// still replay the patch and verify the receiving commit's landing.
// Retention renewal and repackaging must not change this source identity.
func restorationMessage(record Record) string {
	identity := strings.Join([]string{record.RunID, record.RepositoryKey, record.BaseSHA, record.SnapshotSHA, record.PatchDigest}, "\x00")
	return fmt.Sprintf("Restore retained implementation for %s\n\nGoobers-Recovery-Source: sha256:%x", record.RunID, sha256.Sum256([]byte(identity)))
}

func matchesRestorationMessage(record Record, message string) bool {
	message = strings.TrimSpace(message)
	// Older restorations did not carry source identity. They remain eligible
	// for prepared-state replay verification, not identity-only acceptance.
	return message == restorationMessage(record) || message == "Restore retained implementation for "+record.RunID
}
