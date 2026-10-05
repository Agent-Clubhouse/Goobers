package triggerqueue

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/eventing"
)

func causalGroup(t *testing.T, s *Store, count int) (EventGroup, []EventReceipt) {
	t.Helper()
	var receipts []EventReceipt
	var groupID string
	for i := range count {
		receipts = append(receipts, acceptRoutedEvent(t, s, fmt.Sprint(i), childTestTime, eventRoute("merge", "all", time.Second, time.Second, count)))
		result := routeEventTest(t, s, childTestTime)
		groupID = result.Groups[0]
	}
	group, err := s.EventGroup(t.Context(), "web", groupID)
	if err != nil {
		t.Fatal(err)
	}
	return group, receipts
}
func descendantRequest(t *testing.T, s *Store, group EventGroup, id string) EventAcceptance {
	t.Helper()
	producer, err := s.EventConsumerProducer(t.Context(), EventProducer{Gaggle: group.Gaggle, RunID: strings.TrimPrefix(group.AcceptanceID, "trigger-"), Binding: "workflow", Actor: "consumer", Stage: "emit"}, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	req := eventRequest(id, false)
	req.Producer = producer
	req.Plan.Routes = []eventing.Route{eventRoute("next", "", 0, 0, 0)}
	return req
}

func TestEventCausalRootsPropagateAllChargesAtomically(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	group, parents := causalGroup(t, s, 2)
	req := descendantRequest(t, s, group, "descendant")
	receipt, duplicate, err := s.AcceptEvent(t.Context(), req, childTestTime)
	if err != nil || duplicate {
		t.Fatal(receipt, duplicate, err)
	}
	// Exhaust one root. The other must not spend an allowance for a refused
	// consumer even if it sorts first in the root set.
	roots := []string{parents[0].ID, parents[1].ID}
	if roots[0] > roots[1] {
		roots[0], roots[1] = roots[1], roots[0]
	}
	if _, err = s.db.Exec(`UPDATE event_roots SET starts=100 WHERE gaggle='web' AND root_id=?`, roots[1]); err != nil {
		t.Fatal(err)
	}
	result := routeEventTest(t, s, childTestTime)
	if result.State != EventRoutingPartial || len(result.Groups) != 0 {
		t.Fatal(result)
	}
	var starts int
	if err = s.db.QueryRow(`SELECT starts FROM event_roots WHERE gaggle='web' AND root_id=?`, roots[0]).Scan(&starts); err != nil || starts != 1 {
		t.Fatalf("partial root charge=%d %v", starts, err)
	}
	if _, err = s.db.Exec(`UPDATE event_roots SET starts=1 WHERE gaggle='web' AND root_id=?`, roots[1]); err != nil {
		t.Fatal(err)
	}
	req.Envelope = eventRequest("success", false).Envelope
	next, _, err := s.AcceptEvent(t.Context(), req, childTestTime)
	if err != nil {
		t.Fatal(err)
	}
	result = routeEventTest(t, s, childTestTime)
	if len(result.Groups) != 1 {
		t.Fatal(result)
	}
	for _, root := range roots {
		if err = s.db.QueryRow(`SELECT starts FROM event_roots WHERE gaggle='web' AND root_id=?`, root).Scan(&starts); err != nil || starts != 2 {
			t.Fatalf("root %s count=%d %v", root, starts, err)
		}
	}
	nextGroup, err := s.EventGroup(t.Context(), group.Gaggle, result.Groups[0])
	if err != nil {
		t.Fatal(err)
	}
	grandchild := descendantRequest(t, s, nextGroup, "grandchild")
	if grandchild.Producer.Depth != 2 || grandchild.Producer.RootSetDigest != req.Producer.RootSetDigest {
		t.Fatal("descendant reset ancestry")
	}
	if _, _, err = s.AcceptEvent(t.Context(), grandchild, childTestTime); err != nil {
		t.Fatal(err)
	}
	routeEventTest(t, s, childTestTime)
	for _, root := range roots {
		if err = s.db.QueryRow(`SELECT starts FROM event_roots WHERE gaggle='web' AND root_id=?`, root).Scan(&starts); err != nil || starts != 3 {
			t.Fatal(starts, err)
		}
	}
	if _, duplicate, err = s.AcceptEvent(t.Context(), req, childTestTime); err != nil || !duplicate {
		t.Fatal(duplicate, err)
	}
	var count int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM event_receipt_roots WHERE receipt_id=?`, next.ID).Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
}

func TestEventCausalRootsRefuseResetForeignReferenceAndDepth(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	group, _ := causalGroup(t, s, 2)
	cases := map[string]func(*EventProducer){
		"reset":          func(p *EventProducer) { p.RootID = p.RunID; p.RootGroupID = ""; p.CausationID = ""; p.Depth = 0 },
		"foreign-gaggle": func(p *EventProducer) { p.Gaggle = "foreign" },
		"foreign-run":    func(p *EventProducer) { p.RunID = "foreign" },
		"wrong-depth":    func(p *EventProducer) { p.Depth = 2 },
		"body-root":      func(p *EventProducer) { p.RootID = "chosen" },
	}
	before := childTableCount(t, s, "event_receipts")
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			req := descendantRequest(t, s, group, name)
			edit(&req.Producer)
			if _, _, err := s.AcceptEvent(t.Context(), req, childTestTime); err == nil {
				t.Fatal("invalid causal authority accepted")
			}
		})
	}
	if childTableCount(t, s, "event_receipts") != before {
		t.Fatal("refused ancestry created receipt")
	}
	// A historical ordinary workflow also cannot replace its already recorded root.
	req := eventRequest("ordinary", false)
	req.Producer = EventProducer{Gaggle: "web", Binding: "workflow", Actor: "ordinary", RunID: "ordinary", Stage: "emit", RootID: "original"}
	if _, _, err := s.AcceptEvent(t.Context(), req, childTestTime); err != nil {
		t.Fatal(err)
	}
	req.Envelope = eventRequest("ordinary-reset", false).Envelope
	req.Producer.RootID = "reset"
	if _, _, err := s.AcceptEvent(t.Context(), req, childTestTime); !errors.Is(err, ErrEventRootSet) {
		t.Fatal(err)
	}
}

func TestEventCausalRootsCopiedBeforeParentHistoryPrunes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	s := openTestStore(t, path)
	group, parents := causalGroup(t, s, 2)
	req := descendantRequest(t, s, group, "unrouted-descendant")
	descendant, _, err := s.AcceptEvent(t.Context(), req, childTestTime)
	if err != nil {
		t.Fatal(err)
	}
	record, _, err := s.VerifiedEventStart(t.Context(), group.Gaggle, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = dispatchEventTest(t, s, record); err != nil {
		t.Fatal(err)
	}
	if err = s.SettleEventGroup(t.Context(), group.Gaggle, group.ID, strings.TrimPrefix(record.ID, "trigger-"), "completed", childTestTime); err != nil {
		t.Fatal(err)
	}
	// Keep the child receipt fresh while advancing only its parents' retention.
	later := childTestTime.Add(EventRetention + EventTombstoneRetention + time.Hour)
	if _, err = s.db.Exec(`UPDATE event_receipts SET accepted_ns=? WHERE id=?`, later.UnixNano(), descendant.ID); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{childTestTime.Add(EventRetention), later} {
		if _, err = s.PruneEvents(t.Context(), at, 100); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.EventGroup(t.Context(), group.Gaggle, group.ID); err == nil {
		t.Fatal("parent history did not prune")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, path)
	result := routeEventTest(t, s, later)
	if len(result.Groups) != 1 {
		t.Fatal(result)
	}
	for _, parent := range parents {
		var starts int
		if err = s.db.QueryRow(`SELECT starts FROM event_roots WHERE gaggle='web' AND root_id=?`, parent.ID).Scan(&starts); err != nil || starts != 2 {
			t.Fatal(starts, err)
		}
	}
	pins, err := s.EventRootDependencyPage(t.Context(), EventRootDependency{}, 100)
	if err != nil || len(pins) != 2 {
		t.Fatal(pins, err)
	}
}

func TestEventCausalRootSetAndDepthCeilingsFailBeforeAcceptance(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	group, _ := causalGroup(t, s, MaxEventCausalRoots+1)
	_, err := s.EventConsumerProducer(t.Context(), EventProducer{Gaggle: group.Gaggle, RunID: strings.TrimPrefix(group.AcceptanceID, "trigger-"), Binding: "workflow", Actor: "consumer", Stage: "emit"}, group.ID)
	if !errors.Is(err, ErrEventRootSet) {
		t.Fatal(err)
	}
	// Depth derives from all original members, including those suppressed later.
	if _, err = s.db.Exec(`UPDATE event_receipts SET authority=json_set(authority,'$.depth',8,'$.causationId','upstream','$.rootId','root') WHERE id IN (SELECT receipt_id FROM event_deliveries WHERE group_id=?)`, group.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.EventConsumerProducer(t.Context(), EventProducer{Gaggle: group.Gaggle, RunID: strings.TrimPrefix(group.AcceptanceID, "trigger-"), Binding: "workflow", Actor: "consumer", Stage: "emit"}, group.ID)
	if !errors.Is(err, ErrEventRootSet) {
		t.Fatal(err)
	}
}

func TestEventCausalRootDigestDetectsChangedMembership(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	group, _ := causalGroup(t, s, 2)
	req := descendantRequest(t, s, group, "tamper")
	original := req.Producer.RootSetDigest
	req.Producer.RootSetDigest = "sha256:" + strings.Repeat("a", 64)
	if _, _, err := s.AcceptEvent(t.Context(), req, childTestTime); !errors.Is(err, ErrEventRootSet) {
		t.Fatal(err)
	}
	req.Producer.RootSetDigest = original
	receipt, _, err := s.AcceptEvent(t.Context(), req, childTestTime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE event_receipt_roots SET root_id='forged' WHERE receipt_id=? AND root_id=(SELECT MIN(root_id) FROM event_receipt_roots WHERE receipt_id=?)`, receipt.ID, receipt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RouteNextEvent(t.Context(), group.Gaggle, childTestTime); !errors.Is(err, ErrEventRootSet) {
		t.Fatal(err)
	}
	var forged int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM event_roots WHERE root_id='forged'`).Scan(&forged); err != nil || forged != 0 {
		t.Fatal(forged, err)
	}
}
