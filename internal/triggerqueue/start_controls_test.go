package triggerqueue

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/sqliteschema"
)

func controlScope(r Record, source string) StartScope {
	return StartScope{Gaggle: "web", Workflow: "repair", Kind: "test/v1", Source: source, Generation: "archived-generation", PayloadDigest: "sha256:" + childDigest(r.Payload), ReservedRunID: strings.TrimPrefix(r.ID, "trigger-"), Deadline: r.AcceptedAt.Add(time.Hour)}
}
func controlCommand() StartCancellation {
	return StartCancellation{RequestID: "cancel-1", Actor: "verified-human", Reason: "No longer needed", Authority: []byte(`{"issuer":"trusted","subject":"operator"}`)}
}
func pinnedControl(t *testing.T, s *Store, key, source string) StartControl {
	t.Helper()
	r := acceptTest(t, s, key, childTestTime)
	c, err := s.PinStartControl(t.Context(), r.ID, controlScope(r, source))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestStartControlPinsExactArchivedScopeAndSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	s := openTestStore(t, path)
	c := pinnedControl(t, s, "request", "manual")
	for _, change := range []func(*StartScope){func(v *StartScope) { v.Gaggle = "foreign" }, func(v *StartScope) { v.Generation = "reloaded" }, func(v *StartScope) { v.Deadline = v.Deadline.Add(time.Hour) }} {
		scope := c.Scope
		change(&scope)
		if _, err := s.PinStartControl(t.Context(), c.Record.ID, scope); !errors.Is(err, ErrConflict) {
			t.Fatal("repinned accepted scope", err)
		}
	}
	bad := c.Scope
	bad.PayloadDigest = "sha256:" + strings.Repeat("f", 64)
	if _, err := s.PinStartControl(t.Context(), c.Record.ID, bad); !errors.Is(err, ErrTransition) {
		t.Fatal(err)
	}
	if _, err := s.StartControl(t.Context(), "foreign", c.Record.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("foreign custody visible", err)
	}
	got, dup, err := s.RequestStartCancellation(t.Context(), "web", c.Record.ID, controlCommand(), childTestTime.Add(time.Minute))
	if err != nil || dup || got.Record.State != Rejected || got.Disposition != "cancelled" || got.Record.RunID != "" {
		t.Fatal(got, dup, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, path)
	replay, dup, err := s.RequestStartCancellation(t.Context(), "web", c.Record.ID, controlCommand(), childTestTime.Add(24*time.Hour))
	if err != nil || !dup || !replay.CancelRequestedAt.Equal(got.CancelRequestedAt) || replay.Disposition != "cancelled" {
		t.Fatal(replay, dup, err)
	}
	cmd := controlCommand()
	cmd.Authority = []byte(`{"issuer":"trusted","subject":"different-human"}`)
	if _, _, err = s.RequestStartCancellation(t.Context(), "web", c.Record.ID, cmd, childTestTime.Add(time.Minute)); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if err = s.BeginDispatchAt(t.Context(), c.Record.ID, childTestTime); !errors.Is(err, ErrTransition) {
		t.Fatal("cancelled start dispatched", err)
	}
}

func TestStartCancellationRacesDispatchWithoutReleasingAttemptedCustody(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	a, b := openTestStore(t, path), openTestStore(t, path)
	for i := range 20 {
		c := pinnedControl(t, a, fmt.Sprintf("race-%d", i), "manual")
		var dispatchErr, cancelErr error
		var wg sync.WaitGroup
		ready := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-ready
			dispatchErr = a.BeginDispatchAt(t.Context(), c.Record.ID, childTestTime)
		}()
		go func() {
			defer wg.Done()
			<-ready
			_, _, cancelErr = b.RequestStartCancellation(t.Context(), "web", c.Record.ID, controlCommand(), childTestTime.Add(time.Minute))
		}()
		close(ready)
		wg.Wait()
		if cancelErr != nil {
			t.Fatal(cancelErr)
		}
		got, err := a.StartControl(t.Context(), "web", c.Record.ID)
		if err != nil || got.Cancellation == nil {
			t.Fatal(got, err)
		}
		if dispatchErr == nil {
			if got.Record.State != Dispatching || got.Disposition != "" || !got.DisposedAt.IsZero() {
				t.Fatal("attempted cancellation claimed termination", got)
			}
		} else if !errors.Is(dispatchErr, ErrTransition) || got.Record.State != Rejected || got.Disposition != "cancelled" {
			t.Fatal(got, dispatchErr)
		}
	}
}

func TestStartDeadlineExpiresOnlyProvenUnattemptedReceipt(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	c := pinnedControl(t, s, "deadline", "human-restart")
	if got, changed, err := s.ExpireStartControl(t.Context(), "web", c.Record.ID, c.Scope.Deadline.Add(-time.Nanosecond)); err != nil || changed || got.Record.State != Accepted {
		t.Fatal(got, changed, err)
	}
	got, changed, err := s.ExpireStartControl(t.Context(), "web", c.Record.ID, c.Scope.Deadline)
	if err != nil || !changed || got.Record.State != Rejected || got.Disposition != "expired" || got.Cancellation != nil {
		t.Fatal(got, changed, err)
	}
	if _, changed, err = s.ExpireStartControl(t.Context(), "web", c.Record.ID, c.Scope.Deadline.Add(time.Hour)); err != nil || changed {
		t.Fatal(changed, err)
	}
	attempted := pinnedControl(t, s, "uncertain", "direct-engine")
	if err = s.BeginDispatchAt(t.Context(), attempted.Record.ID, childTestTime); err != nil {
		t.Fatal(err)
	}
	if got, changed, err = s.ExpireStartControl(t.Context(), "web", attempted.Record.ID, c.Scope.Deadline.Add(365*24*time.Hour)); err != nil || changed || got.Record.State != Dispatching || got.Disposition != "" {
		t.Fatal(got, changed, err)
	}
	noDeadline := acceptTest(t, s, "child-indefinite", childTestTime)
	scope := controlScope(noDeadline, "child")
	scope.Deadline = time.Time{}
	if _, err = s.PinStartControl(t.Context(), noDeadline.ID, scope); err != nil {
		t.Fatal(err)
	}
	if got, changed, err = s.ExpireStartControl(t.Context(), "web", noDeadline.ID, c.Scope.Deadline.Add(365*24*time.Hour)); err != nil || changed || got.Record.State != Accepted {
		t.Fatal(got, changed, err)
	}
}

func TestTypedStartSettlementRequiresMatchingSourceCustody(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	for _, source := range []string{"child", "session"} {
		c := pinnedControl(t, s, source, source)
		if _, _, err := s.RequestStartCancellation(t.Context(), "web", c.Record.ID, controlCommand(), childTestTime.Add(time.Minute)); !errors.Is(err, ErrTypedStartSettlement) {
			t.Fatal(source, err)
		}
		if _, _, err := s.ExpireStartControl(t.Context(), "web", c.Record.ID, c.Scope.Deadline); !errors.Is(err, ErrTypedStartSettlement) {
			t.Fatal(source, err)
		}
		got, err := s.StartControl(t.Context(), "web", c.Record.ID)
		if err != nil || got.Record.State != Accepted || got.Cancellation != nil || got.Disposition != "" {
			t.Fatal("typed owner changed without settlement", got, err)
		}
	}
}

func TestStartControlsUseBoundedSharedQuotaAndInventory(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	var controls []StartControl
	for i := range 3 {
		controls = append(controls, pinnedControl(t, s, fmt.Sprint(i), "manual"))
	}
	raw := acceptTest(t, s, "unclassified", childTestTime)
	unknown, err := s.UnpinnedStartControls(t.Context(), "", 100)
	if err != nil || len(unknown) != 1 || unknown[0].ID != raw.ID {
		t.Fatal(unknown, err)
	}
	first, err := s.StartControlPage(t.Context(), "web", "", 2)
	if err != nil || len(first) != 2 {
		t.Fatal(first, err)
	}
	last, err := s.StartControlPage(t.Context(), "web", first[1].Record.ID, 2)
	if err != nil || len(last) != 1 {
		t.Fatal(last, err)
	}
	if foreign, err := s.StartControlPage(t.Context(), "foreign", "", 100); err != nil || len(foreign) != 0 {
		t.Fatal(foreign, err)
	}
	if _, err = s.StartControlPage(t.Context(), "web", "", 101); !errors.Is(err, ErrTransition) {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE start_controls SET reserved_bytes=? WHERE acceptance_id=?`, childStoreByteCeiling, raw.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Accept(t.Context(), "over-capacity", "actor", []byte(`{}`), childTestTime); !errors.Is(err, ErrFull) {
		t.Fatal("control reserve was not shared", err)
	}
	// Existing accepted custody can always consume its reserved cancellation bytes.
	command := controlCommand()
	command.Authority = []byte(`{"subject":"` + strings.Repeat("x", 8000) + `"}`)
	command.Actor = strings.Repeat("a", 1024)
	command.Reason = strings.Repeat("r", 512)
	if _, _, err = s.RequestStartCancellation(t.Context(), "web", controls[0].Record.ID, command, childTestTime.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var reserved int
	if err = s.db.QueryRow(`SELECT reserved_bytes FROM start_controls WHERE acceptance_id=?`, controls[0].Record.ID).Scan(&reserved); err != nil || reserved < 0 || reserved >= startControlAllowance {
		t.Fatal(reserved, err)
	}
}

func TestPendingCancellationCannotAgeOutWithFinishedDispatchReceipt(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	c := pinnedControl(t, s, "running", "manual")
	if err := s.BeginDispatchAt(t.Context(), c.Record.ID, childTestTime); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(t.Context(), c.Record.ID, Dispatched, c.Scope.ReservedRunID, "", childTestTime.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.RequestStartCancellation(t.Context(), "web", c.Record.ID, controlCommand(), childTestTime.Add(2*time.Minute))
	if err != nil || got.Record.State != Dispatched || got.Disposition != "" {
		t.Fatal(got, err)
	}
	acceptTest(t, s, "later", childTestTime.Add(ReplayRetention+time.Hour))
	if _, err = s.StartControl(t.Context(), "web", c.Record.ID); err != nil {
		t.Fatal("unfinished cancel was pruned", err)
	}
}

func TestStartControlMigrationPreservesCustodyAndDoesNotInventScope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	index := slices.Index(migrations, startControlSchema)
	if index < 1 {
		t.Fatal("missing migration")
	}
	if err = sqliteschema.Migrate(t.Context(), db, "triggerqueue", migrations[:index]); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO triggers(id,key,actor,payload,state,accepted_ns) VALUES('trigger-0123456789abcdef0123456789abcdef','old','human','{}','dispatching',1)`); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, path)
	unknown, err := s.UnpinnedStartControls(t.Context(), "", 100)
	if err != nil || len(unknown) != 1 || unknown[0].State != Dispatching {
		t.Fatal(unknown, err)
	}
	if page, err := s.StartControlPage(t.Context(), "web", "", 100); err != nil || len(page) != 0 {
		t.Fatal("migration invented gaggle authority", page, err)
	}
	scope := controlScope(unknown[0], "manual")
	if _, err = s.PinStartControl(t.Context(), unknown[0].ID, scope); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`DELETE FROM triggers WHERE id=?`, unknown[0].ID); err != nil {
		t.Fatal(err)
	}
	if count := childTableCount(t, s, "start_controls"); count != 0 {
		t.Fatal("control reserve leaked", count)
	}
}

func TestCorruptStartControlCannotClaimDisposition(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	c := pinnedControl(t, s, "invalid", "manual")
	if _, err := s.db.Exec(`UPDATE start_controls SET disposition='cancelled',disposed_ns=? WHERE acceptance_id=?`, childTestTime.UnixNano(), c.Record.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartControl(t.Context(), "web", c.Record.ID); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
}

func TestCancellationSurvivesCertifiedRequeueAndBlocksAnotherAttempt(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	c := pinnedControl(t, s, "temporary-refusal", "manual")
	if err := s.BeginDispatchAt(t.Context(), c.Record.ID, childTestTime); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RequestStartCancellation(t.Context(), "web", c.Record.ID, controlCommand(), childTestTime.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.Requeue(t.Context(), c.Record.ID, "capacity unavailable"); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginDispatchAt(t.Context(), c.Record.ID, childTestTime); !errors.Is(err, ErrTransition) {
		t.Fatal("requested cancellation relaunched", err)
	}
	got, changed, err := s.ReconcileStartCancellation(t.Context(), "web", c.Record.ID, childTestTime.Add(2*time.Minute))
	if err != nil || !changed || got.Record.State != Rejected || got.Disposition != "cancelled" {
		t.Fatal(got, changed, err)
	}
}

func TestPinnedDeadlineCannotRaceRequeueOrUseLegacyClocklessAdmission(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	c := pinnedControl(t, s, "deadline-requeue", "manual")
	if err := s.BeginDispatch(t.Context(), c.Record.ID); !errors.Is(err, ErrTransition) {
		t.Fatal("clockless claim bypassed deadline", err)
	}
	if err := s.BeginDispatchAt(t.Context(), c.Record.ID, childTestTime); err != nil {
		t.Fatal(err)
	}
	if err := s.Requeue(t.Context(), c.Record.ID, "temporary capacity"); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginDispatchAt(t.Context(), c.Record.ID, c.Scope.Deadline); !errors.Is(err, ErrTransition) {
		t.Fatal("expired requeue dispatched", err)
	}
	got, changed, err := s.ExpireStartControl(t.Context(), "web", c.Record.ID, c.Scope.Deadline)
	if err != nil || !changed || got.Disposition != "expired" {
		t.Fatal(got, changed, err)
	}
}

func TestStartControlMigrationRetainsReservedEventStartCapacity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	s := openTestStore(t, path)
	acceptRoutedEvent(t, s, "open", childTestTime, eventRoute("repair", "all", time.Minute, 2*time.Minute, 3))
	routeEventTest(t, s, childTestTime)
	acceptRoutedEvent(t, s, "pending", childTestTime, eventRoute("immediate", "", 0, 0, 0))
	// Remove only this migration's state from the seeded fixture. Keep later
	// independent migrations installed; lowering schema_meta would replay them.
	for _, query := range []string{`DROP TRIGGER start_control_insert`, `DROP TRIGGER start_control_delete`, `DROP TABLE start_controls`, `UPDATE event_receipts SET reserved_bytes=reserved_bytes-24576*reserved_starts WHERE state='routing_pending'`, `UPDATE event_groups SET reserved_bytes=reserved_bytes-24576*reserved_starts WHERE state='open'`} {
		if _, err := s.db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	for _, migration := range []string{startControlSchema, startControlOutcomeSchema} {
		if _, err := s.db.Exec(migration); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, path)
	var groupReserve int
	if err := s.db.QueryRow(`SELECT reserved_bytes FROM event_groups WHERE state='open'`).Scan(&groupReserve); err != nil || groupReserve != eventGroupAllowance {
		t.Fatal(groupReserve, err)
	}
	routeEventTest(t, s, childTestTime)
	closed, err := s.CloseEventGroups(t.Context(), "web", childTestTime.Add(3*time.Minute), 100)
	if err != nil || len(closed) != 1 {
		t.Fatal(closed, err)
	}
	var starts, reserve int
	if err = s.db.QueryRow(`SELECT COUNT(*),SUM(reserved_bytes) FROM start_controls`).Scan(&starts, &reserve); err != nil || starts != 2 || reserve != 2*startControlAllowance {
		t.Fatal("reserved starts did not transfer once", starts, reserve, err)
	}
}

func TestTypedCustodyCannotBeCancelledByMisclassifyingScope(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	child := acceptChildTest(t, s, childRequest("parent", "stage", "call"), childTestTime)
	record, err := s.ChildStart(t.Context(), child.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PinStartControl(t.Context(), record.ID, controlScope(record, "manual")); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.RequestStartCancellation(t.Context(), "web", record.ID, controlCommand(), childTestTime); !errors.Is(err, ErrTypedStartSettlement) {
		t.Fatal("source label bypassed child ledger", err)
	}
}
