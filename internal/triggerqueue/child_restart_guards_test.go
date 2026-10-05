package triggerqueue

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/sqliteschema"
)

func TestChildRestartAndActualDispositionHaveOneCustodian(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	child, disposition := dispositionFixture(t, s, "parent", childTestTime)
	child, err := s.GetChild(t.Context(), child.Identity)
	if err != nil {
		t.Fatal(err)
	}
	r := childRestartRequest(child, "restart")
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, _, err := s.BeginChildRestart(t.Context(), r, childTestTime.Add(time.Second))
		results <- err
	}()
	go func() {
		defer wg.Done()
		<-start
		_, err := s.RequestChildDisposition(t.Context(), disposition, childTestTime.Add(time.Second))
		results <- err
	}()
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrTransition) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatal("both next execution and prior disposition admitted", successes)
	}
	current, err := s.GetChild(t.Context(), child.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if current.ExecutionEpoch > 0 {
		if _, err = s.RequestChildDisposition(t.Context(), disposition, childTestTime.Add(2*time.Second)); !errors.Is(err, ErrTransition) {
			t.Fatal("old result regained disposition authority", err)
		}
	}
}
func TestChildRestartEnforcesSharedQuotaAndPlanCustody(t *testing.T) {
	for _, mode := range []string{"quota", "large plan", "changed plan", "missing epoch", "missing source result"} {
		t.Run(mode, func(t *testing.T) {
			s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
			c, _ := failedRestartChild(t, s)
			r := childRestartRequest(c, "epoch")
			switch mode {
			case "quota":
				if _, err := s.db.Exec(`UPDATE child_lineages SET reserved_bytes=? WHERE child_id=?`, childStoreByteCeiling, c.ChildID); err != nil {
					t.Fatal(err)
				}
			case "large plan":
				r.Plan = []byte(strings.Repeat("x", MaxChildRestartPlanBytes+1))
				r.PlanDigest = "sha256:" + childDigest(r.Plan)
			case "missing source result":
				if _, err := s.db.Exec(`DELETE FROM child_results WHERE child_id=?`, c.ChildID); err != nil {
					t.Fatal(err)
				}
			}
			_, _, err := s.BeginChildRestart(t.Context(), r, childTestTime.Add(time.Second))
			if mode == "quota" || mode == "large plan" || mode == "missing source result" {
				if err == nil {
					t.Fatal("invalid admission succeeded")
				}
				if childTableCount(t, s, "child_execution_epochs") != 0 {
					t.Fatal("failed admission leaked history")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			query := `UPDATE child_execution_epochs SET plan=x'01' WHERE child_id=? AND epoch=1`
			if mode == "missing epoch" {
				query = `DELETE FROM child_execution_epochs WHERE child_id=? AND epoch=1`
			}
			if _, err = s.db.Exec(query, c.ChildID); err != nil {
				t.Fatal(err)
			}
			called := false
			if err = s.WithChildExecutionResume(t.Context(), c.Identity, r.RunID, func() error { called = true; return nil }); err == nil || called {
				t.Fatal("damaged custody launched", err)
			}
			if _, _, err = s.BeginChildRestart(t.Context(), r, childTestTime.Add(time.Second)); err == nil {
				t.Fatal("damaged admission recreated")
			}
		})
	}
}
func TestChildRestartMigrationPreservesAcceptedIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var prior []string
	for _, migration := range migrations {
		if migration == childRestartSchema {
			break
		}
		prior = append(prior, migration)
	}
	if len(prior) == len(migrations) {
		t.Fatal("restart schema not registered")
	}
	if err = sqliteschema.Migrate(t.Context(), db, "triggerqueue", prior); err != nil {
		t.Fatal(err)
	}
	runID := strings.Repeat("a", 32)
	if _, err = db.Exec(`INSERT INTO child_parents(gaggle,parent_run,created_ns) VALUES('g','parent',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO child_lineages(gaggle,parent_run,occurrence,invocation_key,sequence,child_id,acceptance_id,start_key,actor_digest,payload_digest,state,accepted_ns,updated_ns) VALUES('g','parent','stage','key',1,?,?,'start','actor','payload','running',1,1)`, "child-"+runID, "trigger-"+runID); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, path)
	c, err := s.ChildForRun(t.Context(), runID)
	if err != nil || c.RunID != runID || c.ActiveRunID() != runID || c.ExecutionEpoch != 0 || c.State != ChildRunning {
		t.Fatal(c, err)
	}
}

func TestChildRestartEscalatedSourceRequiresSealedCustody(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	c := acceptChildTest(t, s, childRequest("parent", "stage", "key"), childTestTime)
	if err := s.BeginDispatch(t.Context(), c.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetChildState(t.Context(), c.Identity, ChildStateUpdate{Expected: ChildQueued, State: ChildRunning}, childTestTime); err != nil {
		t.Fatal(err)
	}
	if err := s.SetChildState(t.Context(), c.Identity, ChildStateUpdate{Expected: ChildRunning, State: ChildAwaitingHuman}, childTestTime); err != nil {
		t.Fatal(err)
	}
	c, err := s.GetChild(t.Context(), c.Identity)
	if err != nil {
		t.Fatal(err)
	}
	r := childRestartRequest(c, "human-epoch")
	sealed := childResultValue("sealed escalated source", "preserved workspace")
	r.SourceResultRef = sealed.ReceiptDigest
	if _, _, err = s.BeginChildRestart(t.Context(), r, childTestTime.Add(time.Second)); !errors.Is(err, ErrChildResultPending) {
		t.Fatal("unsealed escalation restarted", err)
	}
	if err = s.KeepChildResult(t.Context(), c, sealed); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.BeginChildRestart(t.Context(), r, childTestTime.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	history, err := s.ChildExecutionHistory(t.Context(), c.Identity)
	if err != nil || len(history) != 2 || history[0].ResultRef != sealed.ReceiptDigest || history[0].State != ChildAwaitingHuman {
		t.Fatal(history, err)
	}
	if _, _, err = s.AcceptChild(t.Context(), childRequest("parent", "stage", "another"), childTestTime.Add(time.Second)); !errors.Is(err, ErrChildSlotOccupied) {
		t.Fatal("restart released unresolved child slot", err)
	}
}
