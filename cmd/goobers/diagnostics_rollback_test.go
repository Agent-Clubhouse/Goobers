//go:build rollbackcompat

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readmodel"
)

// Exercise the actual telemetry --rebuild read-model helper as well as the
// separate library epoch rebuild test. This destructive helper does not retain
// policy floors/tombstones; this fixture deliberately asserts no such promise.
func TestDiagnosticsRollbackCLIRebuild(t *testing.T) {
	root := os.Getenv("GOOBERS_ROLLBACK_FIXTURE")
	if root == "" {
		t.Fatal("use go run ./test/diagnosticsrollback")
	}
	layout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(layout.ReadDB()), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.SchedulerDir(), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, readmodel.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.ReadDB(), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := rebuildReadModel(context.Background(), layout, []string{filepath.Join(root, "runs")}); err != nil {
		t.Fatal(err)
	}
	store, err := readmodel.Open(layout.ReadDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	got, found, err := store.GetRun(context.Background(), "run-0001")
	if err != nil || !found {
		t.Fatalf("rebuilt row found=%v err=%v", found, err)
	}
	data, err = os.ReadFile(filepath.Join(root, "expected.json"))
	if err != nil {
		t.Fatal(err)
	}
	var want readmodel.OperatorFacts
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Operator.RequiredMCP, want.RequiredMCP) || !reflect.DeepEqual(got.Operator.RetryBackoff, want.RetryBackoff) || !reflect.DeepEqual(got.Operator.Activity, want.Activity) {
		t.Fatalf("CLI helper did not restore diagnostics: %+v", got.Operator)
	}
}
