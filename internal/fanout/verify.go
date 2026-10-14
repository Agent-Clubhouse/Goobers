package fanout

import (
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// EffectiveMaxConcurrent is the concurrency bound a map runs with: an unset
// maxConcurrent means 1, as for static maxConcurrentBranches.
func EffectiveMaxConcurrent(m apiv1.Map) int32 {
	if m.MaxConcurrent <= 0 {
		return 1
	}
	return m.MaxConcurrent
}

// MapVisit verifies one journaled map visit against its frozen roster (design
// §8 steps 2-5). Build it from the visit's map.started, feed it every later
// event in sequence order with Observe, and verify a closing map.finished with
// VerifyFinished. Every refusal is an *IntegrityError (workflow_integrity).
type MapVisit struct {
	name     string
	task     string
	items    []journal.MapItem
	started  []bool
	finished []bool
}

// NewMapVisit re-verifies the roster a map.started recorded against the
// pinned map definition: the roster must still be normalized (valid, unique,
// sorted identities with recomputable digests) and the recorded bounds must
// equal the definition's. It never consults the producer.
func NewMapVisit(start journal.Event, def apiv1.Map) (*MapVisit, error) {
	fail := func(kind string, detail string, args ...any) (*MapVisit, error) {
		return nil, &IntegrityError{Kind: kind, FanOut: start.Parallel, Seq: start.Seq, Detail: fmt.Sprintf(detail, args...)}
	}
	if start.Type != journal.EventMapStarted {
		return fail(KindConflicting, "visit opens with %s, not %s", start.Type, journal.EventMapStarted)
	}
	roster := start.MapRoster
	if roster == nil {
		return fail(KindMissing, "map.started carries no frozen roster")
	}
	if start.Parallel != def.Name {
		return fail(KindConflicting, "map.started names map %q, definition is %q", start.Parallel, def.Name)
	}
	if roster.Source.Task != def.Source.Task || roster.Source.Slot != def.Source.Slot {
		return fail(KindConflicting, "roster source %s.%s differs from the pinned source %s.%s",
			roster.Source.Task, roster.Source.Slot, def.Source.Task, def.Source.Slot)
	}
	if roster.FailurePolicy != def.FailurePolicy || roster.ItemTimeoutSeconds != def.ItemTimeoutSeconds ||
		roster.MaxConcurrent != EffectiveMaxConcurrent(def) {
		return fail(KindConflicting, "recorded bounds (%s, %ds, %d workers) differ from the pinned map (%s, %ds, %d workers)",
			roster.FailurePolicy, roster.ItemTimeoutSeconds, roster.MaxConcurrent,
			def.FailurePolicy, def.ItemTimeoutSeconds, EffectiveMaxConcurrent(def))
	}
	if len(roster.Items) > MaxRosterItems {
		return fail(KindExtra, "roster records %d items, bound is %d", len(roster.Items), MaxRosterItems)
	}
	for i, item := range roster.Items {
		if err := verifyRosterItem(start, roster.Items, i, item); err != nil {
			return nil, err
		}
	}
	items := append([]journal.MapItem(nil), roster.Items...)
	return &MapVisit{name: def.Name, task: def.Task, items: items, started: make([]bool, len(items)), finished: make([]bool, len(items))}, nil
}

func verifyRosterItem(start journal.Event, items []journal.MapItem, i int, item journal.MapItem) error {
	fail := func(kind, detail string) error {
		return &IntegrityError{Kind: kind, FanOut: start.Parallel, Branch: i + 1, Seq: start.Seq, Detail: detail}
	}
	if !ValidIdentity(item.Identity) || !ValidDigest(item.Digest) {
		return fail(KindConflicting, fmt.Sprintf("roster item %q has a malformed identity or digest", item.Identity))
	}
	if i > 0 && items[i-1].Identity == item.Identity {
		return fail(KindDuplicate, fmt.Sprintf("roster identity %q is recorded twice", item.Identity))
	}
	if i > 0 && items[i-1].Identity > item.Identity {
		return fail(KindConflicting, fmt.Sprintf("roster identity %q is out of bytewise order", item.Identity))
	}
	digest, err := ItemDigest(item.Identity, item.Item)
	if err != nil || digest != item.Digest {
		return fail(KindConflicting, fmt.Sprintf("roster item %q digest %s does not match its recorded {identity, item}", item.Identity, item.Digest))
	}
	return nil
}

// Name is the map the visit belongs to.
func (v *MapVisit) Name() string { return v.name }

// Items is the frozen roster in branch order: item i runs as branch i+1.
func (v *MapVisit) Items() []journal.MapItem { return append([]journal.MapItem(nil), v.items...) }

// Observe checks one event recorded after the visit's map.started (§8 steps
// 3-5): every branch record must name a roster branch, carry that item's
// identity and digest, follow its own branch.started, and occur once.
func (v *MapVisit) Observe(e journal.Event) error {
	if e.Branch == 0 {
		if e.Type == journal.EventBranchStarted || e.Type == journal.EventBranchFinished {
			return v.refuse(KindConflicting, e, "branch lifecycle record on the root branch")
		}
		return nil
	}
	if e.Branch < 0 || e.Branch > len(v.items) {
		return v.refuse(KindExtra, e, fmt.Sprintf("branch %d is outside the %d-item roster", e.Branch, len(v.items)))
	}
	index := e.Branch - 1
	switch e.Type {
	case journal.EventBranchStarted:
		return v.observeStarted(index, e)
	case journal.EventBranchFinished:
		return v.observeFinished(index, e)
	case journal.EventRunResumed:
		// An intervention that reopens a settled item at @join (the static
		// parallel rule) lets that item settle once more.
		if e.Target == "@join" {
			v.finished[index] = false
		}
		return nil
	case journal.EventStageStarted, journal.EventStageFinished:
		if e.Stage != v.task {
			return v.refuse(KindConflicting, e, fmt.Sprintf("stage %q runs in an item branch of map %q, whose task is %q", e.Stage, v.name, v.task))
		}
		return v.requireOpen(index, e)
	case journal.EventGateStarted, journal.EventGatePaused, journal.EventGateEvaluated:
		return v.requireOpen(index, e)
	default:
		return nil
	}
}

func (v *MapVisit) observeStarted(index int, e journal.Event) error {
	if e.Parallel != v.name {
		return v.refuse(KindConflicting, e, fmt.Sprintf("branch.started names fan-out %q inside map %q", e.Parallel, v.name))
	}
	if v.started[index] {
		return v.refuse(KindDuplicate, e, "item branch started twice")
	}
	if err := v.matchItem(index, e); err != nil {
		return err
	}
	if e.Stage != v.task {
		return v.refuse(KindConflicting, e, fmt.Sprintf("item branch starts at %q, the map task is %q", e.Stage, v.task))
	}
	v.started[index] = true
	return nil
}

func (v *MapVisit) observeFinished(index int, e journal.Event) error {
	if e.Parallel != v.name {
		return v.refuse(KindConflicting, e, fmt.Sprintf("branch.finished names fan-out %q inside map %q", e.Parallel, v.name))
	}
	if !v.started[index] {
		return v.refuse(KindMissing, e, "item branch finished without a branch.started")
	}
	if v.finished[index] {
		return v.refuse(KindDuplicate, e, "item branch finished twice")
	}
	if err := v.matchItem(index, e); err != nil {
		return err
	}
	v.finished[index] = true
	return nil
}

func (v *MapVisit) matchItem(index int, e journal.Event) error {
	item := v.items[index]
	if e.MapItemIdentity != item.Identity || e.MapItemDigest != item.Digest || (e.BranchName != "" && e.BranchName != item.Identity) {
		return v.refuse(KindConflicting, e, fmt.Sprintf("record carries item %q (%s), roster branch %d is %q (%s)",
			e.MapItemIdentity, e.MapItemDigest, index+1, item.Identity, item.Digest))
	}
	return nil
}

func (v *MapVisit) requireOpen(index int, e journal.Event) error {
	if !v.started[index] {
		return v.refuse(KindMissing, e, fmt.Sprintf("%s precedes the item's branch.started", e.Type))
	}
	if v.finished[index] {
		return v.refuse(KindConflicting, e, fmt.Sprintf("%s follows the item's branch.finished", e.Type))
	}
	return nil
}

// VerifyFinished checks a map.finished that closes this visit: one
// completeness entry per roster item, in roster order, each carrying its
// branch id, identity and digest.
func (v *MapVisit) VerifyFinished(e journal.Event) error {
	if e.Type != journal.EventMapFinished || e.Parallel != v.name {
		return v.refuse(KindConflicting, e, fmt.Sprintf("%s for %q does not close map %q", e.Type, e.Parallel, v.name))
	}
	seen := make(map[int]bool, len(e.Completeness))
	for i, outcome := range e.Completeness {
		if seen[outcome.Branch] {
			return v.refuse(KindDuplicate, e, fmt.Sprintf("completeness lists branch %d twice", outcome.Branch))
		}
		seen[outcome.Branch] = true
		if i >= len(v.items) || outcome.Branch < 1 || outcome.Branch > len(v.items) {
			return v.refuse(KindExtra, e, fmt.Sprintf("completeness entry %d names branch %d outside the %d-item roster", i, outcome.Branch, len(v.items)))
		}
		item := v.items[i]
		if outcome.Branch != i+1 || outcome.Name != item.Identity || outcome.MapItemIdentity != item.Identity || outcome.MapItemDigest != item.Digest {
			return v.refuse(KindConflicting, e, fmt.Sprintf("completeness entry %d is branch %d %q (%s), roster branch %d is %q (%s)",
				i, outcome.Branch, outcome.MapItemIdentity, outcome.MapItemDigest, i+1, item.Identity, item.Digest))
		}
	}
	if len(e.Completeness) < len(v.items) {
		return v.refuse(KindMissing, e, fmt.Sprintf("completeness has %d entries for a %d-item roster", len(e.Completeness), len(v.items)))
	}
	return nil
}

func (v *MapVisit) refuse(kind string, e journal.Event, detail string) error {
	return &IntegrityError{Kind: kind, FanOut: v.name, Branch: e.Branch, Seq: e.Seq, Detail: detail}
}
