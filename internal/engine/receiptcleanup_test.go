package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/livejournal"
)

type receiptCleanupWorkspace struct {
	path    string
	removed bool
}

func (w *receiptCleanupWorkspace) Path() string                 { return w.path }
func (w *receiptCleanupWorkspace) Remove(context.Context) error { w.removed = true; return nil }

type receiptCleanupEmitter func(context.Context, livejournal.EmitRequest) (livejournal.EmitResponse, error)

func (f receiptCleanupEmitter) Emit(ctx context.Context, req livejournal.EmitRequest) (livejournal.EmitResponse, error) {
	return f(ctx, req)
}

func TestWorkerReceiptCleanupRequiresHostAcknowledgment(t *testing.T) {
	for _, mode := range []string{"applied", "deduplicated", "failed", "missing-ack", "unwired", "no-receipt"} {
		t.Run(mode, func(t *testing.T) {
			workspace := &receiptCleanupWorkspace{path: t.TempDir()}
			if mode != "no-receipt" {
				if err := os.WriteFile(filepath.Join(workspace.path, "mutations.jsonl"), []byte(`{"receiptId":"receipt-1","provider":"github","kind":"pr","id":"42","operation":"merge"}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			activities := &Activities{}
			if mode != "unwired" {
				activities.Journal = receiptCleanupEmitter(func(_ context.Context, req livejournal.EmitRequest) (livejournal.EmitResponse, error) {
					calls++
					if workspace.removed {
						t.Error("workspace removed before receipt acknowledgment")
					}
					if req.RunID != "owner" || req.Gaggle != "gaggle" || len(req.Ops) != 1 || req.Ops[0].Event.ExternalRef.ID != "42" {
						t.Errorf("wrong receipt destination/content: %+v", req)
					}
					switch mode {
					case "applied":
						return livejournal.EmitResponse{Applied: 1, Seq: 7}, nil
					case "deduplicated":
						return livejournal.EmitResponse{Deduplicated: 1, Seq: 7}, nil
					case "missing-ack":
						return livejournal.EmitResponse{}, nil
					default:
						return livejournal.EmitResponse{}, errors.New("host unavailable")
					}
				})
			}
			activities.removeWorkspaceWithReceipts(t.Context(), apiv1.InvocationEnvelope{RunID: "owner", Gaggle: "gaggle", TaskID: "land"}, workspace)
			wantRemoved := mode == "applied" || mode == "deduplicated" || mode == "no-receipt"
			if workspace.removed != wantRemoved {
				t.Fatalf("removed=%t want=%t calls=%d", workspace.removed, wantRemoved, calls)
			}
			if mode == "no-receipt" && calls != 0 {
				t.Fatal("receipt-free cleanup required host journal")
			}
		})
	}
}
