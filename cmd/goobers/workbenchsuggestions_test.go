package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/internal/workbenchprovider"
	"github.com/goobers/goobers/internal/workbenchservice"
)

func TestWorkbenchHostInstallsSuggestionArtifactReview(t *testing.T) {
	setup, pin := interactiveExecutionFixture(t)
	g := setup.Definitions.Gaggles[0].DeepCopy()
	g.Spec.Project.Branch = "strategy"
	target := interactiveRepository(g.Spec.Project)
	g.Spec.Workbench = &apiv1.GaggleWorkbench{SchemaVersion: "sources/v1", Sources: []apiv1.WorkbenchSource{{Name: "strategy", Kind: "documents", Repository: &target, Paths: []string{"plan.md"}, Writes: &apiv1.WorkbenchWrites{Relationships: []apiv1.WorkbenchRelationship{"references"}}}}}
	g.Spec.InteractiveAccess.Actions = append(g.Spec.InteractiveAccess.Actions, "source.proposeChange")
	if err := setup.InteractiveAccess.Apply([]apiv1.Gaggle{*g}, nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HUMAN_REPO", "human-source-only")
	client := &hostProposalClient{hostDocumentReader: &hostDocumentReader{t: t, target: g.Spec.Project}}
	read := &workbenchservice.Service{Permissions: setup.InteractiveAccess, Repository: func(context.Context, workbenchservice.ReadBinding, interactiveaccess.Credential) (workbenchprovider.RepositoryClient, error) {
		t.Fatal("artifact inspection must not read mutable provider source")
		return client, nil
	}}
	u := &upSession{}
	u.setup, u.l = setup, pin.layout
	u.durableTriggers = acceptedService(t, filepath.Join(pin.layout.SchedulerDir(), "suggestions.db"), newDaemonTriggerService())
	u.installWorkbenchProposals(read, func(context.Context, workbenchservice.ReadBinding, interactiveaccess.Credential) (workbenchprovider.RepositoryProposalClient, error) {
		t.Fatal("review rejection must not mint publication access")
		return client, nil
	})
	id := journal.RunIdentity{RunID: strings.Repeat("e", 32), Gaggle: g.Name, Workflow: "curate", WorkflowVersion: 1, ConfigGeneration: strings.Repeat("f", 64)}
	run, err := journal.Create(pin.layout.ForGaggle(g.Name).RunsDir(), id, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	if err = run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "curate", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	set, err := workbench.BindSources(*g)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := workbench.SourceTargetDigest(set.Scope, set.Sources[0])
	if err != nil {
		t.Fatal(err)
	}
	endpoint := func(suffix string) workbench.SuggestionEndpoint {
		return workbench.SuggestionEndpoint{Ref: &workbench.NodeRef{GaggleID: g.Name, SourceBindingID: "strategy", Kind: "objective-document", SourceID: "obj-00000000-0000-0000-0000-00000000000" + suffix}, Evidence: &workbench.SuggestionEvidence{SourceTargetDigest: digest, Path: "plan.md", RepositoryRevision: &workbench.SuggestionRepositoryRevision{Commit: strings.Repeat("a", 40), BlobID: strings.Repeat("b", 40), ContentDigest: strings.Repeat("c", 64)}}}
	}
	raw, _ := json.Marshal(workbench.RelationshipSuggestions{SchemaVersion: workbench.SuggestionSchemaVersion, Suggestions: []workbench.RelationshipSuggestion{{Kind: "references", From: endpoint("1"), To: endpoint("2"), Rationale: "Review these source relationships."}}})
	if _, err = run.RecordStageArtifact("curate", 1, "", "suggestions.json", raw); err != nil {
		t.Fatal(err)
	}
	p := httpapi.Principal{Issuer: "issuer", Subject: "human", Roles: []httpapi.Role{httpapi.RoleOperate}}
	opts := append(u.apiHandlerOpts, httpapi.WithAuthenticator(interactiveTestAuthenticator{principal: &p}))
	handler, err := httpapi.NewHandler(&readservice.Local{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), opts...)
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/gaggles/" + g.Name + "/workbench/suggestions/"
	call := func(method, route string, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, base+route, strings.NewReader(string(body)))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	response := call(http.MethodGet, "artifacts/"+id.RunID, nil)
	var inventory workbench.SuggestionInventory
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &inventory) != nil || len(inventory.Artifacts) != 1 {
		t.Fatal(response.Code, response.Body)
	}
	selection := workbench.SuggestionSelection{RunID: id.RunID, Sequence: inventory.Artifacts[0].Sequence}
	response = call(http.MethodGet, "artifacts/"+id.RunID+"/"+strconv.FormatUint(selection.Sequence, 10), nil)
	var batch workbench.SuggestionBatch
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &batch) != nil || len(batch.Candidates) != 1 {
		t.Fatal(response.Code, response.Body)
	}
	request := workbench.SuggestionDecisionRequest{Selection: selection, Key: batch.Candidates[0].Suggestion.Key, Decision: "reject", Reason: "Not relevant to this objective."}
	raw, _ = json.Marshal(request)
	var review workbench.SuggestionReview
	for range 2 {
		response = call(http.MethodPost, "decisions", raw)
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &review) != nil || review.State != "rejected" || review.Proposal != nil {
			t.Fatal(response.Code, response.Body)
		}
	}
	response = call(http.MethodGet, "reviews/"+review.ID, nil)
	if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(response.Code, response.Body)
	}
	if _, err = u.openWorkbenchSuggestionRun(t.Context(), "foreign", id.RunID); err == nil {
		t.Fatal("foreign run artifact accepted")
	}
	g.Spec.InteractiveAccess = nil
	if err = setup.InteractiveAccess.Apply([]apiv1.Gaggle{*g}, nil); err != nil {
		t.Fatal(err)
	}
	if denied := call(http.MethodGet, "reviews/"+review.ID, nil); denied.Code != 403 {
		t.Fatal("retained review bypassed current source access", denied.Code, denied.Body)
	}
}
