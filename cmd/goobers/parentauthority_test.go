package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type parentAuthorityFixture struct {
	service       *daemonCredentialService
	run           *journal.Run
	contract      childpod.Contract
	digest, token string
	scoped        childpod.ParentAttemptBlobs
	base          blobstore.Store
	handler       http.Handler
}

func newParentAuthorityFixture(t *testing.T) parentAuthorityFixture {
	t.Helper()
	f := newPinnedChildFixture(t)
	run, env := configuredChildStage(t, f)
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	id, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	start := events[len(events)-1]
	q, err := triggerqueue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	instanceLog, _, err := journal.OpenInstanceLog(f.layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instanceLog.Close() })
	s := newDaemonCredentialService(f.layout, f.cfg, nil, journal.NewRegistryScrubber(), instanceLog).withStageGrants(f.layout.Root, "127.0.0.1:8080", false)
	t.Cleanup(func() { unregisterDaemonStageGrants(f.layout.Root, s) })
	s.Replace(credentialPlaneDefinitionsFromSet(f.applied))
	if err = s.enableChildWorkflows(q, f.applied); err != nil {
		t.Fatal(err)
	}
	contract := childpod.Contract{Version: 1, Identity: id, ParentOrigin: env.ChildWorkflowOrigin, Stage: "plan", Attempt: 1, PodAttempt: int(start.Seq), StartedAt: start.Time, Ceiling: credentials.NewChildCeiling(false, env.Capabilities, env.Capabilities)}
	raw, err := json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	digest := journal.Digest(raw)
	store := childpod.ParentBlobs{RunDir: run.Dir(), Identity: id}
	if err = store.Put(t.Context(), digest, raw); err != nil {
		t.Fatal(err)
	}
	if err = store.BindContract(t.Context(), digest); err != nil {
		t.Fatal(err)
	}
	token, err := s.grants.key.MintWorkflowParentPod(id.RunID, digest, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := podauth.NewAuthenticator(s.grants.key, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	base, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writer, err := livejournal.NewWriter(func(string) (string, bool) { return f.layout.ForGaggle(id.Gaggle).RunsDir(), true }, livejournal.WithSpanSource(base))
	if err != nil {
		t.Fatal(err)
	}
	release, err := writer.Adopt(id.RunID, id.Gaggle, run)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { release(); writer.Close() })
	surrender, err := dispatcher.NewSurrenderDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), httpapi.WithAuthenticator(auth), httpapi.WithCredentialService(s), httpapi.WithBlobService(s.childBlobPlane(base)), httpapi.WithJournalService(containedJournalPlane{JournalService: writer, service: s}), httpapi.WithSurrenderService(containedSurrenderPlane{SurrenderDir: surrender, service: s}))
	if err != nil {
		t.Fatal(err)
	}
	return parentAuthorityFixture{s, run, contract, digest, token, childpod.ParentAttemptBlobs{Store: store, ContractDigest: digest}, base, handler}
}
func (f parentAuthorityFixture) request(method, path string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	f.handler.ServeHTTP(out, req)
	return out
}
func TestParentAuthorityHTTPGrantAndAttemptCustody(t *testing.T) {
	f := newParentAuthorityFixture(t)
	path := "/api/v1/runs/" + f.contract.Identity.RunID + "/child-workflow-access"
	body, _ := json.Marshal(map[string]string{"contractDigest": f.digest})
	out := f.request(http.MethodPost, path, body)
	if out.Code != 200 || out.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("acquire %d %s", out.Code, out.Body)
	}
	var access httpapi.ChildWorkflowAccessResponse
	if err := json.Unmarshal(out.Body.Bytes(), &access); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(f.service.shared.Scrub([]byte(access.BearerToken)), []byte(access.BearerToken)) {
		t.Fatal("unregistered secret")
	}
	if _, err := f.service.grants.key.VerifyChildWorkflowGrant(access.BearerToken); err != nil {
		t.Fatal(err)
	}

	replay := f.request(http.MethodPost, path, body)
	if replay.Code != 200 || !bytes.Equal(replay.Body.Bytes(), out.Body.Bytes()) {
		t.Fatalf("uncertain grant delivery changed: %d %s", replay.Code, replay.Body)
	}
	wrong, _ := json.Marshal(map[string]string{"contractDigest": journal.Digest([]byte("other"))})
	if out = f.request(http.MethodPost, path, wrong); out.Code != 400 {
		t.Fatal("foreign contract accepted", out.Code)
	}
	foreign := []byte("private sibling")
	foreignDigest := journal.Digest(foreign)
	if err := f.base.Put(t.Context(), foreignDigest, foreign); err != nil {
		t.Fatal(err)
	}
	if err := f.scoped.Store.Put(t.Context(), foreignDigest, foreign); err != nil {
		t.Fatal(err)
	}
	if out = f.request(http.MethodGet, dispatcher.BlobPathPrefix+foreignDigest, nil); out.Code != 404 {
		t.Fatalf("sibling read %d %s", out.Code, out.Body)
	}
	owned := []byte("own diagnostic")
	ownedDigest := journal.Digest(owned)
	if out = f.request(http.MethodPut, dispatcher.BlobPathPrefix+ownedDigest, owned); out.Code != 204 {
		t.Fatalf("put %d %s", out.Code, out.Body)
	}
	if _, err := f.base.Get(t.Context(), ownedDigest); err == nil {
		t.Fatal("shared publication")
	}
	if out = f.request(http.MethodDelete, path, body); out.Code != 200 {
		t.Fatalf("revoke %d %s", out.Code, out.Body)
	}
	if out = f.request(http.MethodPost, path, body); out.Code == 200 {
		t.Fatal("revoked attempt renewed")
	}
	ordinary, err := f.service.grants.key.Mint(f.contract.Identity.RunID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	f.token = ordinary
	if out = f.request(http.MethodPost, path, body); out.Code != 403 {
		t.Fatal("ordinary pod minted parent grant")
	}
}
func TestParentAuthorityHTTPJournalAndSurrenderStayInAttempt(t *testing.T) {
	f := newParentAuthorityFixture(t)
	stage := f.contract.Stage
	run := f.contract.Identity.RunID
	data := []byte("parent artifact")
	emit := livejournal.EmitRequest{RunID: run, Gaggle: f.contract.Identity.Gaggle, Ops: []livejournal.Op{{Kind: livejournal.OpArtifact, Key: "test/artifact", Time: time.Now(), Artifact: &livejournal.ArtifactOp{Stage: stage, Attempt: 1, Name: "artifact", Data: data}}}}
	raw, _ := json.Marshal(emit)
	path := "/api/v1/runs/" + run + "/journal/emit"
	if out := f.request(http.MethodPost, path, raw); out.Code != 200 {
		t.Fatalf("emit %d %s", out.Code, out.Body)
	}

	// Artifact references use the same request-local boundary as inline bytes.
	foreign := []byte("private shared artifact")
	foreignRef, err := journal.ArtifactRef(foreign)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.base.Put(t.Context(), foreignRef.Digest, foreign); err != nil {
		t.Fatal(err)
	}
	refEmit := livejournal.EmitRequest{RunID: run, Gaggle: f.contract.Identity.Gaggle, Ops: []livejournal.Op{{Kind: livejournal.OpArtifact, Key: "test/foreign-ref", Time: time.Now(), Artifact: &livejournal.ArtifactOp{Stage: stage, Attempt: 1, Name: "foreign", Ref: &foreignRef}}}}
	refRaw, _ := json.Marshal(refEmit)
	if out := f.request(http.MethodPost, path, refRaw); out.Code == 200 {
		t.Fatal("journal adopted a shared artifact outside the attempt")
	}

	for _, kind := range []string{"isolated.parent.writer.started", "isolated.parent.writer.joined", "isolated.child.writer.started", "isolated.child.writer.joined", "isolated.pod.apply-planned"} {
		control := livejournal.EmitRequest{RunID: run, Gaggle: f.contract.Identity.Gaggle, Ops: []livejournal.Op{{Kind: livejournal.OpAppend, Key: "forged/" + kind, Time: time.Now(), Event: &journal.Event{Type: journal.EventRunnerAnnotation, Stage: stage, Attempt: 1, Runner: map[string]any{"kind": kind}}}}}
		controlRaw, _ := json.Marshal(control)
		if out := f.request(http.MethodPost, path, controlRaw); out.Code == 200 {
			t.Fatal("pod forged host writer custody", kind)
		}
	}
	digest := journal.Digest(data)
	if _, err := f.scoped.Get(t.Context(), digest); err != nil {
		t.Fatal("scoped publication missing", err)
	}
	if _, err := f.base.Get(t.Context(), digest); err == nil {
		t.Fatal("journal escaped to shared store")
	}
	emit.Ops[0].Artifact.Stage = "sibling"
	raw, _ = json.Marshal(emit)
	if out := f.request(http.MethodPost, path, raw); out.Code == 200 {
		t.Fatal("journal sibling accepted")
	}
	output, _ := json.Marshal(childpod.Output{Version: 1, ContractDigest: f.digest})
	outputDigest := journal.Digest(output)
	if err := f.scoped.Put(t.Context(), outputDigest, output); err != nil {
		t.Fatal(err)
	}
	surrendered := dispatcher.SurrenderedResult{RecoveryAcknowledged: true, ChildWorkspaceDigest: outputDigest, Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}}
	raw, _ = json.Marshal(surrendered)
	path = fmt.Sprintf("/api/v1/runs/%s/stages/%s/attempts/%d/surrender", run, stage, f.contract.PodAttempt)
	if out := f.request(http.MethodPost, path, raw); out.Code != 200 {
		t.Fatalf("surrender %d %s", out.Code, out.Body)
	}
	if out := f.request(http.MethodPost, strings.Replace(path, "/plan/", "/other/", 1), raw); out.Code == 200 {
		t.Fatal("foreign surrender accepted")
	}
	if err := f.run.Append(journal.Event{Type: journal.EventStageFinished, Stage: stage, Attempt: 1, Status: string(apiv1.ResultSuccess)}); err != nil {
		t.Fatal(err)
	}
	if out := f.request(http.MethodGet, dispatcher.BlobPathPrefix+digest, nil); out.Code == 200 {
		t.Fatal("ended stage retained HTTP custody")
	}
}

// Keep the fixture's contract validity independent of the HTTP adapter.
func TestParentJournalCaptureKeysAreSeparatedBySignedAttempt(t *testing.T) {
	f := newParentAuthorityFixture(t)
	a := parentAttemptCustody{contract: f.contract, digest: f.digest}
	req := livejournal.EmitRequest{RunID: f.contract.Identity.RunID, Gaggle: f.contract.Identity.Gaggle, Ops: []livejournal.Op{{Key: strings.Repeat("a", 32) + "/open", Checkpoint: &livejournal.TranscriptCheckpointOp{Capture: strings.Repeat("a", 32), Stage: "plan"}}}}
	first, err := parentJournalRequest(a, req)
	if err != nil {
		t.Fatal(err)
	}
	a.digest = journal.Digest([]byte("next-attempt"))
	second, err := parentJournalRequest(a, req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Ops[0].Key == second.Ops[0].Key || req.Ops[0].Checkpoint.Capture != strings.Repeat("a", 32) {
		t.Fatal("capture namespace aliases another attempt or mutates caller")
	}
}

func TestParentAuthorityHTTPCredentialsStopAtCancellation(t *testing.T) {
	f := newParentAuthorityFixture(t)
	var resolved []string
	f.service.buildSources = func(credentialGaggleScope) (credentials.Resolver, []credentials.Grant, error) {
		functions := map[string]credentials.ResolveFunc{}
		grants := []credentials.Grant{}
		for _, key := range []string{"agent:model", "repo:read", "repo:push", "mcp:vendor"} {
			functions[key] = func(context.Context) (string, error) {
				resolved = append(resolved, key)
				return "secret-parent-" + key, nil
			}
			grants = append(grants, credentials.Grant{Capability: key, Ref: key})
		}
		resolver, err := credentials.NewResolverWithExpiring(nil, nil, functions, nil)
		return resolver, grants, err
	}
	raw, _ := json.Marshal(httpapi.CredentialResolveRequest{RunID: f.contract.Identity.RunID, Stage: f.contract.Stage})
	out := f.request(http.MethodPost, "/api/v1/credentials/resolve", raw)
	if out.Code != 200 {
		t.Fatalf("resolve %d %s", out.Code, out.Body)
	}
	var response httpapi.CredentialResolveResponse
	if err := json.Unmarshal(out.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(resolved, []string{"agent:model"}) || len(response.Credentials) != 1 || response.Credentials[0].Capability != "agent:model" {
		t.Fatalf("credential ceiling widened: %+v %v", response, resolved)
	}
	if err := f.service.childQueue.FenceChildParent(t.Context(), triggerqueue.ChildParent{Gaggle: f.contract.Identity.Gaggle, ParentRunID: f.contract.Identity.RunID}, "operator", time.Now()); err != nil {
		t.Fatal(err)
	}
	if out = f.request(http.MethodPost, "/api/v1/credentials/resolve", raw); out.Code == 200 {
		t.Fatal("cancelled parent received credentials")
	}
	if len(resolved) != 1 {
		t.Fatal("cancelled parent reached secret source")
	}
}

func TestParentJournalAcceptsOnlyOwnHarnessTaskAlias(t *testing.T) {
	f := newParentAuthorityFixture(t)
	a := parentAttemptCustody{contract: f.contract, digest: f.digest}
	task := f.contract.Identity.RunID + ":" + f.contract.Stage
	req := livejournal.EmitRequest{RunID: f.contract.Identity.RunID, Gaggle: f.contract.Identity.Gaggle, Ops: []livejournal.Op{{Kind: livejournal.OpAppend, Key: "alias", Event: &journal.Event{Type: journal.EventRunnerAnnotation, Stage: task}}}}
	normalized, err := parentJournalRequest(a, req)
	if err != nil || normalized.Ops[0].Event.Stage != f.contract.Stage || normalized.Ops[0].Event.Attempt != 1 || req.Ops[0].Event.Stage != task || req.Ops[0].Event.Attempt != 0 {
		t.Fatalf("own alias: %+v %v", normalized, err)
	}
	req.Ops[0].Event.Stage = "other-run:" + f.contract.Stage
	if _, err = parentJournalRequest(a, req); err == nil {
		t.Fatal("foreign task alias accepted")
	}
}

func TestContainedSurrenderVerifiesDeclaredArtifactCustody(t *testing.T) {
	f := newParentAuthorityFixture(t)
	output, _ := json.Marshal(childpod.Output{Version: 1, ContractDigest: f.digest})
	outputDigest := journal.Digest(output)
	if err := f.scoped.Put(t.Context(), outputDigest, output); err != nil {
		t.Fatal(err)
	}
	data := []byte("returned evidence")
	ref, err := journal.ArtifactRef(data)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.scoped.Put(t.Context(), ref.Digest, data); err != nil {
		t.Fatal(err)
	}
	pointer := apiv1.ArtifactPointer{Path: ref.Path, Digest: ref.Digest, Size: ref.Size, Integrity: apiv1.IntegrityDerived}
	baseline := dispatcher.SurrenderedResult{RecoveryAcknowledged: true, ChildWorkspaceDigest: outputDigest, Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Artifacts: []apiv1.ArtifactPointer{pointer}}}
	for _, tc := range []struct {
		name          string
		edit          func(*dispatcher.SurrenderedResult)
		review, valid bool
	}{
		{name: "own artifact", valid: true},
		{name: "wrong size", edit: func(out *dispatcher.SurrenderedResult) { out.Result.Artifacts[0].Size++ }},
		{name: "artifact trust", edit: func(out *dispatcher.SurrenderedResult) { out.Result.Artifacts[0].Integrity = apiv1.IntegrityTrusted }},
		{name: "result trust", edit: func(out *dispatcher.SurrenderedResult) { out.Result.Integrity = apiv1.IntegrityMaintainer }},
		{name: "task cannot verdict", edit: func(out *dispatcher.SurrenderedResult) { out.Verdict = &apiv1.Verdict{Decision: "pass"} }},
		{name: "review requires verdict", review: true},
		{name: "review own evidence", review: true, valid: true, edit: func(out *dispatcher.SurrenderedResult) {
			out.Verdict = &apiv1.Verdict{Decision: "pass", Evidence: []apiv1.ArtifactPointer{pointer}}
		}},
		{name: "review foreign evidence", review: true, edit: func(out *dispatcher.SurrenderedResult) {
			foreign := pointer
			foreign.Digest = journal.Digest([]byte("sibling"))
			out.Verdict = &apiv1.Verdict{Decision: "pass", Evidence: []apiv1.ArtifactPointer{foreign}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := baseline
			out.Result.Artifacts = append([]apiv1.ArtifactPointer(nil), baseline.Result.Artifacts...)
			if tc.edit != nil {
				tc.edit(&out)
			}
			raw, _ := json.Marshal(out)
			err := validateContainedSurrender(t.Context(), f.contract, f.digest, f.scoped, tc.review, raw)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%t: %v", tc.valid, err)
			}
		})
	}
}
