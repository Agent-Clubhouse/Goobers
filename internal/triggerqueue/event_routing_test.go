package triggerqueue

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/eventing"
)

func eventRoute(consumer, mode string, window, maxWait time.Duration, maxEvents int) eventing.Route {
	r := eventing.Route{Consumer: consumer, Revision: "revision-1", Workflow: consumer, WorkflowDigest: "workflow-1", GooberDigest: "goober-1", ConfigGeneration: "generation-1"}
	if mode != "" {
		r.Debounce = &eventing.Debounce{Key: "pr:42", Window: window, MaxWait: maxWait, MaxEvents: maxEvents, InputMode: mode}
	}
	return r
}

func acceptRoutedEvent(t *testing.T, s *Store, id string, at time.Time, routes ...eventing.Route) EventReceipt {
	t.Helper()
	req := eventRequest(id, false)
	req.Plan.Routes = routes
	receipt, duplicate, err := s.AcceptEvent(t.Context(), req, at)
	if err != nil || duplicate {
		t.Fatalf("accept: %+v %v %v", receipt, duplicate, err)
	}
	return receipt
}

func routeEventTest(t *testing.T, s *Store, at time.Time) EventRouteResult {
	t.Helper()
	result, err := s.RouteNextEvent(t.Context(), "web", at)
	if err != nil || !result.Found {
		t.Fatalf("route: %+v %v", result, err)
	}
	return result
}

func TestEventRoutingIndependentConsumersAndImmutableInputs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	s := openTestStore(t, path)
	now := childTestTime
	routes := []eventing.Route{eventRoute("immediate", "", 0, 0, 0), eventRoute("all", "all", 5*time.Second, 6*time.Second, 100), eventRoute("latest", "latest", 2*time.Second, 6*time.Second, 100)}
	one := acceptRoutedEvent(t, s, "1", now, routes...)
	pins, err := s.EventDependencyPage(t.Context(), "", 100)
	if err != nil || len(pins) != 1 || fmt.Sprint(pins[0].ConfigGenerations) != "[generation-1]" {
		t.Fatalf("pre-route dependency pins: %+v %v", pins, err)
	}
	first := routeEventTest(t, s, now)
	two := acceptRoutedEvent(t, s, "2", now.Add(time.Second), routes...)
	second := routeEventTest(t, s, now.Add(time.Second))
	if first.Groups[0] == second.Groups[0] || first.Groups[1] != second.Groups[1] || first.Groups[2] != second.Groups[2] {
		t.Fatal("consumer input sets crossed")
	}
	closed, err := s.CloseEventGroups(t.Context(), "web", now.Add(3*time.Second), 100)
	if err != nil || len(closed) != 1 || closed[0].Route.Consumer != "latest" {
		t.Fatalf("latest close: %+v %v", closed, err)
	}
	oldLatest := closed[0]
	three := acceptRoutedEvent(t, s, "3", now.Add(3*time.Second), routes...)
	third := routeEventTest(t, s, now.Add(3*time.Second))
	if third.Groups[2] == first.Groups[2] || third.Groups[1] != first.Groups[1] {
		t.Fatal("closed group absorbed an event")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, path)
	closed, err = s.CloseEventGroups(t.Context(), "web", now.Add(6*time.Second), 100)
	if err != nil || len(closed) != 2 {
		t.Fatalf("reopened timer: %+v %v", closed, err)
	}
	all, err := s.EventGroup(t.Context(), "web", first.Groups[1])
	if err != nil || all.Count != 3 || !all.Deadline.Equal(now.Add(6*time.Second)) {
		t.Fatalf("max wait not pinned: %+v %v", all, err)
	}
	record, start, err := s.VerifiedEventStart(t.Context(), "web", all.ID)
	if err != nil || start.InputMode != "all" || start.EventCount != 3 || start.ConfigGeneration != "generation-1" || len(record.Payload) > MaxPayloadBytes {
		t.Fatalf("pinned start: %+v %v", start, err)
	}
	if strings.Contains(string(record.Payload), `"data"`) {
		t.Fatal("start inlined event payloads")
	}
	members, err := s.EventMembers(t.Context(), "web", all.ID, 0, 100)
	if err != nil || len(members) != 3 || members[0].ReceiptID != one.ID || members[1].ReceiptID != two.ID || members[2].ReceiptID != three.ID {
		t.Fatalf("ordered all members: %+v %v", members, err)
	}
	members, err = s.EventMembers(t.Context(), "web", oldLatest.ID, 0, 100)
	if err != nil || len(members) != 2 || members[0].Selected || !members[1].Selected {
		t.Fatalf("latest lost suppressed history: %+v %v", members, err)
	}
	if _, err = s.EventInput(t.Context(), "web", oldLatest.ID, three.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("nonmember hydration accepted", err)
	}
	if _, err = s.EventGroup(t.Context(), "foreign", all.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("cross-gaggle group read", err)
	}
	if _, _, err = s.VerifiedEventStart(t.Context(), "foreign", all.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("cross-gaggle start read", err)
	}
	if result, err := s.RouteNextEvent(t.Context(), "web", now.Add(7*time.Second)); err != nil || result.Found {
		t.Fatal("reopen replayed receipt", result, err)
	}
	if got := childTableCount(t, s, "triggers"); got != 6 {
		t.Fatalf("starts=%d", got)
	}
}

func TestEventDelayedRoutingKeepsOnTimeMembershipAndCountClose(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	now := childTestTime
	route := eventRoute("all", "all", 5*time.Second, 20*time.Second, 2)
	one := acceptRoutedEvent(t, s, "1", now, route)
	acceptRoutedEvent(t, s, "2", now.Add(time.Second), route)
	first := routeEventTest(t, s, now.Add(10*time.Second))
	if groups, err := s.CloseEventGroups(t.Context(), "web", now.Add(10*time.Second), 100); err != nil || len(groups) != 0 {
		t.Fatal("timer raced ahead of accepted receipt", groups, err)
	}
	second := routeEventTest(t, s, now.Add(10*time.Second))
	if first.Groups[0] != second.Groups[0] {
		t.Fatal("routing delay split on-time events")
	}
	group, err := s.EventGroup(t.Context(), "web", first.Groups[0])
	if err != nil || group.Count != 2 || group.CloseReason != "count" || !group.ClosedAt.Equal(now.Add(time.Second)) {
		t.Fatalf("count close: %+v %v", group, err)
	}
	input, err := s.EventInput(t.Context(), "web", group.ID, one.ID)
	if err != nil || string(input.Envelope) != string(one.Envelope) {
		t.Fatal("envelope custody changed", err)
	}
}

func TestEventRouteTransactionRollsBackAndConcurrentReplayKeepsOneStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	left := openTestStore(t, path)
	now := childTestTime
	receipt := acceptRoutedEvent(t, left, "1", now, eventRoute("repair", "", 0, 0, 0))
	if _, err := left.db.Exec(`CREATE TRIGGER fail_event_start BEFORE INSERT ON triggers BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := left.RouteNextEvent(t.Context(), "web", now); err == nil {
		t.Fatal("injected failure lost")
	}
	if childTableCount(t, left, "event_groups") != 0 || childTableCount(t, left, "event_deliveries") != 0 || childTableCount(t, left, "event_roots") != 0 {
		t.Fatal("partial routing transaction committed")
	}
	retained, err := left.Event(t.Context(), "web", "ingress", receipt.ID)
	if err != nil || retained.State != EventRoutingPending {
		t.Fatal("failed routing lost custody", retained, err)
	}
	if _, err := left.db.Exec(`DROP TRIGGER fail_event_start`); err != nil {
		t.Fatal(err)
	}
	right := openTestStore(t, path)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			s := left
			if i%2 != 0 {
				s = right
			}
			if _, err := s.RouteNextEvent(t.Context(), "web", now); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if childTableCount(t, left, "triggers") != 1 || childTableCount(t, left, "event_groups") != 1 || childTableCount(t, left, "event_deliveries") != 1 {
		t.Fatal("concurrent replay created duplicates")
	}
}

func TestEventDependenciesOutliveRoutingAndDispatch(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	now := childTestTime
	receipt := acceptRoutedEvent(t, s, "1", now, eventRoute("repair", "", 0, 0, 0))
	result := routeEventTest(t, s, now)
	record, start, err := s.VerifiedEventStart(t.Context(), "web", result.Groups[0])
	if err != nil {
		t.Fatal(err)
	}
	if err = dispatchEventTest(t, s, record); err != nil {
		t.Fatal(err)
	}
	later := now.Add(365 * 24 * time.Hour)
	if _, err = s.PruneEvents(t.Context(), later, 100); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Accept(t.Context(), "ordinary", "operator", []byte(`{}`), later); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.VerifiedEventStart(t.Context(), "web", start.GroupID); err != nil {
		t.Fatal("generic pruning erased live consumer start", err)
	}
	retained, err := s.Event(t.Context(), "web", "ingress", receipt.ID)
	if err != nil || len(retained.Envelope) == 0 {
		t.Fatal("dispatch acknowledgement released event input", err)
	}
	if err = s.SettleEventGroup(t.Context(), "web", start.GroupID, "foreign", "completed", later); !errors.Is(err, ErrTransition) {
		t.Fatal("foreign terminal identity released inputs", err)
	}
	runID := strings.TrimPrefix(record.ID, "trigger-")
	if err = s.SettleEventGroup(t.Context(), "web", start.GroupID, runID, "completed", later); err != nil {
		t.Fatal(err)
	}
	pruned, err := s.PruneEvents(t.Context(), later.Add(EventRetention), 100)
	if err != nil || pruned.Tombstoned != 1 {
		t.Fatalf("terminal dependent retention: %+v %v", pruned, err)
	}
	if _, err = s.EventInput(t.Context(), "web", start.GroupID, receipt.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("expired input remained hydratable", err)
	}
	for range 3 {
		if _, err = s.PruneEvents(t.Context(), later.Add(EventRetention+EventTombstoneRetention), 100); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"event_receipts", "event_deliveries", "event_groups", "event_roots", "event_group_roots"} {
		if n := childTableCount(t, s, table); n != 0 {
			t.Fatalf("retention leaked %s=%d", table, n)
		}
	}
}

func dispatchEventTest(t *testing.T, s *Store, record Record) error {
	t.Helper()
	if err := s.BeginDispatch(t.Context(), record.ID); err != nil {
		return err
	}
	return s.Finish(t.Context(), record.ID, Dispatched, strings.TrimPrefix(record.ID, "trigger-"), "", record.AcceptedAt)
}

func TestEventReservationsProtectStartsFromOtherIntake(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	now := childTestTime
	acceptRoutedEvent(t, s, "1", now, eventRoute("repair", "", 0, 0, 0))
	_, err := s.db.Exec(`WITH RECURSIVE n(v) AS (VALUES(1) UNION ALL SELECT v+1 FROM n WHERE v<?) INSERT INTO triggers(id,key,actor,payload,state,accepted_ns) SELECT printf('trigger-%032x',v),printf('fill-%d',v),'fixture',X'7b7d','accepted',? FROM n`, MaxRecords-1, now.UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	// This fixture isolates the record-count quota from the independent byte reserve.
	if _, err := s.db.Exec(`UPDATE start_controls SET reserved_bytes=0`); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Accept(t.Context(), "ordinary", "operator", []byte(`{}`), now); !errors.Is(err, ErrFull) {
		t.Fatal("manual intake stole reserved slot", err)
	}
	if _, _, err = s.AcceptChild(t.Context(), childRequest("parent", "stage", "call"), now); !errors.Is(err, ErrFull) {
		t.Fatal("child intake stole reserved slot", err)
	}
	routeEventTest(t, s, now)
	if got := childTableCount(t, s, "triggers"); got != MaxRecords {
		t.Fatalf("reserved closure did not fit: %d", got)
	}
}

func TestEventRootBudgetAndBadKeyPreserveOtherDeliveries(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	now := childTestTime
	base := eventRequest("seed", false)
	base.Producer = EventProducer{Gaggle: "web", Binding: "workflow", Actor: "run:source", RunID: "source", Stage: "emit", RootID: "root", CausationID: "upstream", Depth: 1}
	for i := range MaxEventRootStarts - 1 {
		req := base
		req.Envelope = eventRequest(fmt.Sprint(i), false).Envelope
		req.Plan.Routes = []eventing.Route{eventRoute("repair", "", 0, 0, 0)}
		if _, _, err := s.AcceptEvent(t.Context(), req, now); err != nil {
			t.Fatal(err)
		}
		routeEventTest(t, s, now)
	}
	base.Plan.Routes = []eventing.Route{eventRoute("last", "", 0, 0, 0), eventRoute("overflow", "", 0, 0, 0), eventRoute("missing-key", "all", time.Second, 2*time.Second, 100)}
	base.Plan.Routes[2].Debounce.Key = ""
	base.Plan.Routes[2].FailureReason = "debounce key is missing"
	receipt, _, err := s.AcceptEvent(t.Context(), base, now)
	if err != nil {
		t.Fatal(err)
	}
	result := routeEventTest(t, s, now)
	if result.State != EventRoutingPartial || len(result.Groups) != 1 {
		t.Fatalf("partial delivery: %+v", result)
	}
	deliveries, err := s.EventDeliveries(t.Context(), "web", receipt.ID)
	if err != nil || len(deliveries) != 3 {
		t.Fatal(deliveries, err)
	}
	if deliveries[0].GroupID == "" || deliveries[1].Reason != "debounce key is missing" || deliveries[2].Reason != ErrEventChainLimit.Error() {
		t.Fatalf("delivery reasons: %+v", deliveries)
	}
	if childTableCount(t, s, "triggers") != MaxEventRootStarts {
		t.Fatal("root start budget bypassed")
	}
	base.Producer.RootID = ""
	if _, _, err = s.AcceptEvent(t.Context(), base, now); err == nil {
		t.Fatal("workflow producer reset root")
	}
}

func TestEventWorkflowRootBudgetOutlivesReceiptRetention(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	now := childTestTime
	req := eventRequest("1", false)
	req.Producer = EventProducer{Gaggle: "web", Binding: "workflow", Actor: "run:source", RunID: "source", Stage: "emit", RootID: "root"}
	req.Plan.Routes = []eventing.Route{eventRoute("repair", "", 0, 0, 0)}
	if _, _, err := s.AcceptEvent(t.Context(), req, now); err != nil {
		t.Fatal(err)
	}
	result := routeEventTest(t, s, now)
	record, start, err := s.VerifiedEventStart(t.Context(), "web", result.Groups[0])
	if err != nil {
		t.Fatal(err)
	}
	if err = dispatchEventTest(t, s, record); err != nil {
		t.Fatal(err)
	}
	if err = s.SettleEventGroup(t.Context(), "web", start.GroupID, strings.TrimPrefix(record.ID, "trigger-"), "completed", now); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{now.Add(EventRetention), now.Add(EventRetention + EventTombstoneRetention)} {
		if _, err = s.PruneEvents(t.Context(), at, 100); err != nil {
			t.Fatal(err)
		}
	}
	if childTableCount(t, s, "event_receipts") != 0 || childTableCount(t, s, "event_roots") != 1 {
		t.Fatal("receipt expiry reset a workflow root budget")
	}
	pins, err := s.EventRootDependencyPage(t.Context(), EventRootDependency{}, 1)
	if err != nil || len(pins) != 1 || pins[0] != (EventRootDependency{Gaggle: "web", RootID: "root", SourceRunID: "source"}) {
		t.Fatalf("root source pin lost: %+v %v", pins, err)
	}
	if tail, err := s.EventRootDependencyPage(t.Context(), pins[0], 1); err != nil || len(tail) != 0 {
		t.Fatal("root page cursor replayed reference", tail, err)
	}
	later := now.Add(EventRetention + EventTombstoneRetention + time.Hour)
	req.Envelope = eventRequest("2", false).Envelope
	if _, _, err = s.AcceptEvent(t.Context(), req, later); err != nil {
		t.Fatal(err)
	}
	routeEventTest(t, s, later)
	var starts int
	if err = s.db.QueryRow(`SELECT starts FROM event_roots WHERE gaggle='web' AND root_id='root'`).Scan(&starts); err != nil || starts != 2 {
		t.Fatalf("long-lived root budget reset: %d %v", starts, err)
	}
}

func TestEventTimerReplayAndTamperedStartFailClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	left := openTestStore(t, path)
	right := openTestStore(t, path)
	now := childTestTime
	acceptRoutedEvent(t, left, "1", now, eventRoute("repair", "latest", time.Second, time.Minute, 10))
	result := routeEventTest(t, left, now)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			s := left
			if i%2 != 0 {
				s = right
			}
			if _, err := s.CloseEventGroups(t.Context(), "web", now.Add(time.Second), 1); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if childTableCount(t, left, "triggers") != 1 {
		t.Fatal("timer replay duplicated start")
	}
	record, start, err := left.VerifiedEventStart(t.Context(), "web", result.Groups[0])
	if err != nil {
		t.Fatal(err)
	}
	if err = dispatchEventTest(t, left, record); err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(record.Payload), "generation-1", "generation-2", 1)
	if _, err = left.db.Exec(`UPDATE triggers SET payload=? WHERE id=?`, []byte(changed), record.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err = left.VerifiedEventStart(t.Context(), "web", start.GroupID); !errors.Is(err, ErrConflict) {
		t.Fatal("mutable queue payload changed pinned execution", err)
	}
	if err = left.SettleEventGroup(t.Context(), "web", start.GroupID, strings.TrimPrefix(record.ID, "trigger-"), "completed", now.Add(time.Minute)); !errors.Is(err, ErrConflict) {
		t.Fatal("tampered start released source custody", err)
	}
}
