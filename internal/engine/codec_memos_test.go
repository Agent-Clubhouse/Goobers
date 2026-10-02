package engine

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/converter"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/temporalcodec"
)

type codecMemoClient struct{ *completedRunFake }

func (c codecMemoClient) DescribeWorkflowExecution(context.Context, string, string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	return nil, serviceerror.NewNotFound("not found")
}

func memoCodec(t *testing.T) converter.DataConverter {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "history"), 0700); err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "history", "v1.pem"), pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600); err != nil {
		t.Fatal(err)
	}
	dc, err := temporalcodec.DataConverter(&instance.Config{SecretStores: []instance.SecretStoreConfig{{Name: "keys", Kind: instance.SecretStoreKindFileKey, Directory: root}}, Temporal: &instance.TemporalConfig{PayloadCodec: &instance.PayloadCodecConfig{KeyRef: &instance.KeyRef{Store: "keys", Name: "history", Version: "v1"}}}})
	if err != nil {
		t.Fatal(err)
	}
	return dc
}

func TestSealedAndMixedMemoReaders(t *testing.T) {
	dc := memoCodec(t)
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "sealed", true: "mixed"}[legacy], func(t *testing.T) {
			spec := crSpec("implement", []apiv1.Task{crTask("implement", "")}, nil)
			proj := executeForProjection(t, projectionInput("codec-memo", spec), &Activities{Det: &scriptedStages{}, Workspaces: testWorkspaces(t)}, false)
			gaggle, err := dc.ToPayload("web")
			if err != nil {
				t.Fatal(err)
			}
			workflowDC := dc
			if legacy {
				workflowDC = converter.GetDefaultDataConverter()
			}
			name, err := workflowDC.ToPayload("implementation")
			if err != nil {
				t.Fatal(err)
			}
			fake := &completedRunFake{projection: proj, executions: []*workflowpb.WorkflowExecutionInfo{{Execution: &commonpb.WorkflowExecution{WorkflowId: proj.Identity.RunID}, Memo: &commonpb.Memo{Fields: map[string]*commonpb.Payload{RunGaggleMemoKey: gaggle, RunWorkflowMemoKey: name}}}}}
			owned := map[string]struct{}{"web": {}}
			open, err := NewWorkflowLiveness(codecMemoClient{fake}, "default", dc).OpenRuns(context.Background(), owned)
			if err != nil || len(open) != 1 {
				t.Fatalf("sealed liveness memo: %v %v", open, err)
			}
			if open[proj.Identity.RunID].WorkflowID != proj.Identity.RunID {
				t.Fatal("run inverse lost")
			}
			control, err := NewWorkflowLiveness(codecMemoClient{fake}, "default").OpenRuns(context.Background(), owned)
			if err != nil || len(control) != 0 {
				t.Fatal("default converter unexpectedly decoded sealed memo")
			}
			reconciler, err := NewCompletedRunReconciler(fake, "default", map[string]string{"web": t.TempDir()}, nil, dc)
			if err != nil {
				t.Fatal(err)
			}
			count, err := reconciler.Reconcile(context.Background())
			if err != nil || count != 1 || fake.queries < 1 {
				t.Fatalf("sealed completed-run memo: count=%d queries=%d err=%v", count, fake.queries, err)
			}
		})
	}
}
