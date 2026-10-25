//go:build cloudintegration

package childengine

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childresult"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/objstore"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/workerhost"
)

// Scenario 9 exercises the production pod fetch and executor context resolver
// with a real receipt, then the same continuation after retention and blob GC.
func TestChildQualificationRetiredReceipt(t *testing.T) {
	h := newQualificationHarness(t)
	run := h.StartParent()
	var result engine.RunResult
	if err := run.Get(h.ctx, &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != engine.StatusCompleted || result.Outputs["plan"].Summary != "parent read child receipt through blob plane" {
		t.Fatalf("parent result: %+v", result)
	}
	parent := triggerqueue.ChildParent{Gaggle: "example", ParentRunID: run.GetID()}
	children, err := h.epoch.store.Children(h.ctx, parent, "", 100)
	if err != nil || len(children) != 1 {
		t.Fatalf("children=%+v: %v", children, err)
	}
	child := children[0]
	key, err := podauth.LoadKeyRingFile(h.keyFile, "")
	if err != nil {
		t.Fatal(err)
	}
	token, err := key.MintScoped(run.GetID(), time.Minute, podauth.ScopeBlob)
	if err != nil {
		t.Fatal(err)
	}
	blobs := &dispatcher.BlobClient{BaseURL: h.server.URL, Token: token}
	receipt, err := blobs.Get(h.ctx, child.ResultRef)
	if err != nil {
		t.Fatal(err)
	}
	pointer := childresult.ContinuationPointer(child.ResultRef, int64(len(receipt)))
	t.Run("retained_receipt_resolves", func(t *testing.T) {
		if err := qualificationResolveReceipt(t, h.ctx, blobs, pointer, receipt); err != nil {
			t.Fatal(err)
		}
	})
	if t.Failed() {
		return
	}
	if err := h.epoch.store.AcknowledgeChild(h.ctx, child.Identity, child.ResultRef, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := h.epoch.store.MarkChildParentSettled(h.ctx, parent, time.Now()); err != nil {
		t.Fatal(err)
	}
	if pruned, err := h.epoch.store.PruneChildren(h.ctx, time.Now(), 100); err != nil || pruned.Tombstoned != 0 {
		t.Fatalf("prune inside retention=%+v: %v", pruned, err)
	}
	// Advance only this family's durable retention timestamp; production prune
	// still uses the database clock and all of its ordinary custody checks.
	if _, err := h.pool.Exec(h.ctx, `UPDATE control.child_parent SET settled_at=clock_timestamp()-interval '31 days' WHERE instance_id=$1 AND parent_run_id=$2`, testInstance, run.GetID()); err != nil {
		t.Fatal(err)
	}
	if pruned, err := h.epoch.store.PruneChildren(h.ctx, time.Now(), 100); err != nil || pruned.Tombstoned != 1 {
		t.Fatalf("prune after retention=%+v: %v", pruned, err)
	}
	qualificationCollectReceipt(t, h, child.ResultRef)
	t.Run("collected_receipt_names_retirement", func(t *testing.T) {
		err := qualificationResolveReceipt(t, h.ctx, blobs, pointer, receipt)
		if !errors.Is(err, childresult.ErrResultRetired) || !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("old continuation error=%v; want ErrResultRetired wrapping the absent receipt", err)
		}
	})
	t.Run("ordinary_missing_artifact_is_not_retirement", func(t *testing.T) {
		ordinary := pointer
		ordinary.Name = "ordinary-result"
		err := qualificationResolveReceipt(t, h.ctx, blobs, ordinary, receipt)
		if !errors.Is(err, fs.ErrNotExist) || errors.Is(err, childresult.ErrResultRetired) {
			t.Fatalf("ordinary missing artifact error=%v", err)
		}
	})
}

func qualificationResolveReceipt(t *testing.T, ctx context.Context, blobs *dispatcher.BlobClient, pointer apiv1.ContextPointer, receipt []byte) error {
	t.Helper()
	// Each invocation gets a cold node cache; an old local copy must not mask GC.
	root, workspace := t.TempDir(), t.TempDir()
	if err := workerhost.MaterializeContext(ctx, blobs, root, []apiv1.ContextPointer{pointer}); err != nil {
		return err
	}
	scrubber := journal.NewRegistryScrubber()
	resolver, err := credentials.NewResolver(nil)
	if err != nil {
		t.Fatal(err)
	}
	injector, err := credentials.NewInjector(resolver, nil, scrubber)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := workerhost.NewStagingArtifacts(root, scrubber, nil)
	adapter := &harness.FakeAdapter{Act: func(_ context.Context, req harness.RunRequest) error {
		got, err := os.ReadFile(filepath.Join(req.Workspace, ".goobers", "context", "00-"+pointer.Name))
		if err != nil {
			return err
		}
		if !bytes.Equal(got, receipt) {
			return errors.New("resolved receipt differs from sealed child result")
		}
		return harness.WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "receipt resolved"})
	}}
	executor, err := harness.NewExecutor(adapter, injector, artifacts, artifacts, harness.NewContextResolver(artifacts, root), scrubber, "")
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Invoke(ctx, apiv1.InvocationEnvelope{RunID: "receipt-reader", TaskID: "read", WorkflowID: "receipt-reader", Gaggle: "example", Goal: "read child receipt", Workspace: workspace, ContextPointers: []apiv1.ContextPointer{pointer}})
	if err == nil && result.Status != apiv1.ResultSuccess {
		t.Fatalf("resolver stage result=%+v", result)
	}
	return err
}

func qualificationCollectReceipt(t *testing.T, h *qualificationHarness, digest string) {
	t.Helper()
	// Enable the normal two-observation collector only for this isolated store.
	metadata, err := objstore.NewMetadataWithOptions(h.pool, h.blobs, objstore.MetadataOptions{ExperimentalPhysicalDelete: true, MaxPublishLatency: time.Hour, PITRRetention: objstore.DefaultPITRRetention})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(h.ctx, `INSERT INTO control.blob_reference_writer_protocol (writer_class,protocol_version) VALUES ('journal',1),('recovery',1),('child-custody',1) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	var referenced bool
	if err := h.pool.QueryRow(h.ctx, `SELECT control.blob_is_referenced($1)`, digest).Scan(&referenced); err != nil || referenced {
		t.Fatalf("retired receipt referenced=%v: %v", referenced, err)
	}
	if _, err := h.pool.Exec(h.ctx, `INSERT INTO control.blob_gc_candidate (digest,unreferenced_at) VALUES ($1,clock_timestamp()-interval '40 days')`, digest); err != nil {
		t.Fatal(err)
	}
	manager, err := lease.New(lease.Config{Pool: h.pool, InstanceID: testInstance, Name: "gc", Holder: "qualification-receipt-gc"})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Acquire(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Release(ctx, handle); err != nil {
			t.Error(err)
		}
	}()
	if deleted, err := metadata.SweepGC(h.ctx, manager, handle, time.Hour); err != nil || deleted != 0 {
		t.Fatalf("first GC observation deleted=%d: %v", deleted, err)
	}
	if _, err := h.pool.Exec(h.ctx, `UPDATE control.blob_gc_candidate SET first_observed_at=clock_timestamp()-interval '2 hours' WHERE digest=$1`, digest); err != nil {
		t.Fatal(err)
	}
	if deleted, err := metadata.SweepGC(h.ctx, manager, handle, time.Hour); err != nil || deleted != 1 {
		t.Fatalf("mature GC deleted=%d: %v", deleted, err)
	}
	if present, err := h.blobs.Has(h.ctx, digest); err != nil || present {
		t.Fatalf("collected receipt present=%v: %v", present, err)
	}
}
