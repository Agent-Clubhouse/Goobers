package childpod

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

type dispatchFunc func(context.Context, dispatcher.Attempt, []dispatcher.RunnerSpec) (dispatcher.Report, error)

func (f dispatchFunc) Dispatch(ctx context.Context, a dispatcher.Attempt, r []dispatcher.RunnerSpec) (dispatcher.Report, error) {
	return f(ctx, a, r)
}

type recordFake struct{ data map[string][]byte }

func (r *recordFake) RecordArtifact(name string, data []byte) (journal.Ref, error) {
	if r.data == nil {
		r.data = map[string][]byte{}
	}
	r.data[name] = append([]byte(nil), data...)
	return journal.Ref{Digest: journal.Digest(data)}, nil
}

func requestFixture() Request {
	digest := "sha256:" + strings.Repeat("a", 64)
	id := journal.RunIdentity{Schema: journal.RunSchema, InstanceID: "instance", RunID: strings.Repeat("1", 32), Workflow: "generated", Gaggle: "gaggle", ConfigGeneration: digest, WorkflowDigest: digest, GooberDigest: digest, StartedAt: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)}
	id.Child = &journal.ChildLineage{Gaggle: id.Gaggle, ParentRunID: strings.Repeat("2", 32), ParentWorkflow: "parent", StageOccurrence: "occurrence", InvocationKey: "key", AcceptanceID: "trigger-" + id.RunID, SourceDigest: digest, EnvelopeDigest: digest}
	return Request{Identity: id, Attempt: dispatcher.Attempt{InstanceID: id.InstanceID, RunID: id.RunID, Gaggle: id.Gaggle, Workflow: id.Workflow, Stage: "stage", Number: 1, PodAttempt: 11, Workspace: "scratch"}, StartedAt: id.StartedAt, Ceiling: credentials.NewChildCeiling(false, nil, nil)}
}

func TestExecutorRequiresPodWriterProofBeforeConsumingResult(t *testing.T) {
	for _, joined := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown", true: "joined"}[joined], func(t *testing.T) {
			r := requestFixture()
			blobs, err := blobstore.NewDir(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			plane, err := dispatcher.NewSurrenderDir(filepath.Join(t.TempDir(), "surrender"))
			if err != nil {
				t.Fatal(err)
			}
			recorder := &recordFake{}
			executor := Executor{Blobs: blobs, Surrenders: plane, Recorder: recorder}
			executor.Dispatcher = dispatchFunc(func(ctx context.Context, a dispatcher.Attempt, _ []dispatcher.RunnerSpec) (dispatcher.Report, error) {
				data, err := blobs.Get(ctx, a.ChildExecutionDigest)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := DecodeContract(data, a.ChildExecutionDigest); err != nil {
					t.Fatal(err)
				}
				data, _ = json.Marshal(Output{Version: 1, ContractDigest: a.ChildExecutionDigest})
				digest := journal.Digest(data)
				if err = blobs.Put(ctx, digest, data); err != nil {
					t.Fatal(err)
				}
				data, _ = json.Marshal(dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, ChildWorkspaceDigest: digest})
				if err = plane.Put(ctx, a.RunID, a.Stage, a.PodAttempt, data); err != nil {
					t.Fatal(err)
				}
				return dispatcher.Report{ChildCreateAttempted: true, ChildPodUID: "exact", WorkspaceWritersStopped: joined, SurrenderConfirmed: true}, nil
			})
			ctx, err := credentials.WithChildCeiling(t.Context(), r.Ceiling)
			if err != nil {
				t.Fatal(err)
			}
			ctx, proof := invoke.WithWorkspaceQuiescence(ctx)
			_, _, err = executor.Execute(ctx, r)
			if joined {
				if err != nil || proof.Verify() != nil {
					t.Fatal(err, proof.Verify())
				}
			} else {
				if !errors.Is(err, dispatcher.ErrChildIsolation) || proof.Verify() == nil {
					t.Fatal("unproved pod accepted", err)
				}
				if len(recorder.data) != 1 {
					t.Fatal("consumed output before stopped proof")
				}
			}
		})
	}
}

func TestContractRejectsPublicationAndSubstitution(t *testing.T) {
	r := requestFixture()
	c, _, err := makeContract(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(c)
	if _, err := DecodeContract(data, journal.Digest([]byte("foreign"))); err == nil {
		t.Fatal("substitution accepted")
	}
	c.Ceiling.AllowPublication = true
	if c.Validate() == nil {
		t.Fatal("synthetic history publication accepted")
	}
}
