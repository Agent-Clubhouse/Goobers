package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

func TestParentLateHTTPCustodyRequiresOriginalUnjoinedWorker(t *testing.T) {
	f := newParentAuthorityFixture(t)
	run, stage := f.contract.Identity.RunID, f.contract.Stage
	mark := func(kind string) {
		t.Helper()
		if err := f.run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: stage, Attempt: f.contract.Attempt, Runner: map[string]any{"kind": kind, "contractDigest": f.digest}}); err != nil {
			t.Fatal(err)
		}
	}
	mark(parentPodWriterStarted)
	for _, e := range []journal.Event{
		{Type: journal.EventStageFinished, Stage: stage, Attempt: f.contract.Attempt, Status: string(apiv1.ResultFailure)},
	} {
		if err := f.run.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	output, _ := json.Marshal(childpod.Output{Version: 1, ContractDigest: f.digest})
	digest := journal.Digest(output)
	if out := f.request(http.MethodPut, dispatcher.BlobPathPrefix+digest, output); out.Code != 204 {
		t.Fatalf("late output upload: %d %s", out.Code, out.Body)
	}
	annotation := livejournal.EmitRequest{RunID: run, Gaggle: f.contract.Identity.Gaggle, Ops: []livejournal.Op{{Kind: livejournal.OpArtifact, Key: "late/diagnostic", Time: time.Now(), Artifact: &livejournal.ArtifactOp{Stage: stage, Attempt: f.contract.Attempt, Name: "late-diagnostic", Data: []byte("worker stopped")}}}}
	raw, _ := json.Marshal(annotation)
	if out := f.request(http.MethodPost, "/api/v1/runs/"+run+"/journal/emit", raw); out.Code != 200 {
		t.Fatalf("late diagnostic: %d %s", out.Code, out.Body)
	}
	if err := f.run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)}); err != nil {
		t.Fatal(err)
	}
	annotation.Ops[0].Key = "late/after-terminal"
	raw, _ = json.Marshal(annotation)
	if out := f.request(http.MethodPost, "/api/v1/runs/"+run+"/journal/emit", raw); out.Code != http.StatusConflict {
		t.Fatalf("terminal journal accepted fresh observation: %d %s", out.Code, out.Body)
	}
	if out := f.request(http.MethodGet, dispatcher.BlobPathPrefix+digest, nil); out.Code != 200 {
		t.Fatalf("terminal run lost pending output custody: %d %s", out.Code, out.Body)
	}
	surrendered := dispatcher.SurrenderedResult{RecoveryAcknowledged: true, ChildWorkspaceDigest: digest, Result: apiv1.ResultEnvelope{Status: apiv1.ResultFailure, Error: &apiv1.ErrorInfo{Code: "stopped", Message: "worker stopped"}}}
	raw, _ = json.Marshal(surrendered)
	path := fmt.Sprintf("/api/v1/runs/%s/stages/%s/attempts/%d/surrender", run, stage, f.contract.PodAttempt)
	if out := f.request(http.MethodPost, path, raw); out.Code != 200 {
		t.Fatalf("late surrender: %d %s", out.Code, out.Body)
	}
	cred, _ := json.Marshal(httpapi.CredentialResolveRequest{RunID: run, Stage: stage})
	if out := f.request(http.MethodPost, "/api/v1/credentials/resolve", cred); out.Code == 200 {
		t.Fatal("late custody restored credentials")
	}
	grant, _ := json.Marshal(map[string]string{"contractDigest": f.digest})
	if out := f.request(http.MethodPost, "/api/v1/runs/"+run+"/child-workflow-access", grant); out.Code == 200 {
		t.Fatal("late custody restored child execution authority")
	}
	mark(parentPodWriterJoined)
	if out := f.request(http.MethodPost, path, raw); out.Code == 200 {
		t.Fatal("joined worker retained surrender custody")
	}
	if out := f.request(http.MethodGet, dispatcher.BlobPathPrefix+digest, nil); out.Code == 200 {
		t.Fatal("joined worker retained blob custody")
	}
}
