package engine

import (
	"fmt"

	"go.temporal.io/sdk/workflow"

	"github.com/goobers/goobers/internal/livejournal"
)

// journalCommitOwner serializes emissions from independent branch recorders.
// Its queue is ordinary deterministic workflow state, never a native goroutine
// or mutex. Serial walks without human decision ownership leave owner nil;
// versioned fan-out Run assembly
// installs it before the root or any branch can append journal operations.
//
// emitted is the acknowledged prefix. submitted also protects an uncertain
// prefix: a failed activity may have committed before losing its response.
// Rollback cannot remove that prefix or change its retry keys.
type journalCommitOwner struct {
	root      *runJournal
	mu        workflow.Mutex
	entries   []branchJournalEntry
	emitted   int
	submitted int
	ordinals  map[string]int
	receipts  map[string]livejournal.AppendReceipt
}

type branchJournalEntry struct {
	recorder *runJournal
	index    int
}

func (r *runJournal) trackOwnedOp() {
	if r.owner != nil {
		r.owner.entries = append(r.owner.entries, branchJournalEntry{recorder: r, index: len(r.proj.Ops) - 1})
	}
}

// projectionSnapshot includes accepted and pending decisions in their original
// workflow order. Only branch-local, never-submitted rollback may remove them.
// This retains terminal repair evidence when terminal delivery fails.
func (r *runJournal) projectionSnapshot() JournalProjection {
	if r.owner == nil {
		return r.proj
	}
	out := r.owner.root.proj
	out.Ops = make([]JournalOp, len(r.owner.entries))
	for i, entry := range r.owner.entries {
		out.Ops[i] = entry.recorder.proj.Ops[entry.index]
	}
	return out
}

func (o *journalCommitOwner) rollbackFloor(r *runJournal, floor int) int {
	for _, entry := range o.entries[:o.submitted] {
		if entry.recorder == r {
			floor = max(floor, entry.index+1)
		}
	}
	return floor
}

func (o *journalCommitOwner) rollback(r *runJournal, floor int) {
	kept := o.entries[:0]
	for _, entry := range o.entries {
		if entry.recorder != r || entry.index < floor {
			kept = append(kept, entry)
		}
	}
	o.entries = kept
}

func (o *journalCommitOwner) emitPending(ctx workflow.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := o.mu.Lock(ctx); err != nil {
		return err
	}
	defer o.mu.Unlock()
	pending := len(o.entries)
	if o.emitted >= pending {
		return nil
	}
	if !o.root.live {
		return fmt.Errorf("engine: branch commit ownership requires live journaling")
	}
	req := o.request(pending)
	// Set before yielding to the activity. Other branch coroutines may append
	// or roll back while it is pending, but cannot remove this submitted prefix.
	o.submitted = max(o.submitted, pending)
	var resp livejournal.EmitResponse
	if err := workflow.ExecuteActivity(emitActivityContext(ctx), ActEmitJournal, req).Get(ctx, &resp); err != nil {
		return fmt.Errorf("engine: emit owned journal ops for run %s: %w", req.RunID, err)
	}
	count := len(req.Ops)
	if resp.Applied < 0 || resp.Applied > count || resp.Deduplicated < 0 || resp.Deduplicated > count || resp.Applied+resp.Deduplicated != count {
		return fmt.Errorf("engine: incomplete owned journal acknowledgement for run %s", req.RunID)
	}
	if err := o.preserveAppendReceipts(req.Ops, resp.AppendReceipts, resp.Seq); err != nil {
		return err
	}
	for _, entry := range o.entries[o.emitted:pending] {
		entry.recorder.emitted = max(entry.recorder.emitted, entry.index+1)
	}
	o.emitted = pending
	return nil
}

func (o *journalCommitOwner) assignEmitKeys(pending int) {
	if o.ordinals == nil {
		o.ordinals = make(map[string]int)
	}
	for _, entry := range o.entries[:pending] {
		op := &entry.recorder.proj.Ops[entry.index]
		if op.EmitKey != "" {
			continue
		}
		assignJournalKey(o.root.proj.Identity.RunID, o.ordinals, op)
	}
}

func (o *journalCommitOwner) request(pending int) livejournal.EmitRequest {
	o.assignEmitKeys(pending)
	root := o.root
	req := livejournal.EmitRequest{RunID: root.proj.Identity.RunID, Gaggle: root.proj.Identity.Gaggle}
	for _, entry := range o.entries[o.emitted:pending] {
		req.Ops = append(req.Ops, liveOpFrom(entry.recorder.proj.Ops[entry.index]))
	}
	if o.emitted == 0 {
		req.Open = &livejournal.OpenHeader{Identity: root.proj.Identity, Item: root.proj.Item, Graph: root.proj.Graph, Definition: root.proj.Definition, GateGooberCapabilities: root.proj.GateGooberCapabilities}
	}
	return req
}
