package triggerqueue

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/sqliteschema"
)

func TestChildPublicationMigrationPinsExistingEmitterWithoutChangingIntent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var prior []string
	for _, migration := range migrations {
		if migration == childPublicationExecutionSchema {
			break
		}
		prior = append(prior, migration)
	}
	if len(prior) == len(migrations) {
		t.Fatal("publication emitter migration missing")
	}
	if err = sqliteschema.Migrate(t.Context(), db, "triggerqueue", prior); err != nil {
		t.Fatal(err)
	}
	runID := strings.Repeat("a", 32)
	intent := []byte(`{"original":"immutable intent"}`)
	digest := "sha256:" + childDigest(intent)
	if _, err = db.Exec(`INSERT INTO child_parents(gaggle,parent_run,created_ns) VALUES('g','parent',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO child_lineages(gaggle,parent_run,occurrence,invocation_key,sequence,child_id,acceptance_id,start_key,actor_digest,payload_digest,state,accepted_ns,updated_ns,publication_pending,publication_branch_digest) VALUES('g','parent','stage','key',1,?,?,'start','actor','payload','running',1,1,1,?)`, "child-"+runID, "trigger-"+runID, digest); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO child_publications(child_id,action,intent,digest,state,receipt,created_ns,updated_ns) VALUES(?,'branch',?,?,'effect_pending',x'',1,1)`, "child-"+runID, intent, digest); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, path)
	child, err := s.ChildForRun(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	record, err := s.ChildPublication(t.Context(), child.Identity, "branch")
	if err != nil || record.ExecutionRunID != runID || record.Digest != digest || string(record.Intent) != string(intent) || record.State != "effect_pending" {
		t.Fatal("migration relabeled effect custody", record, err)
	}
}
