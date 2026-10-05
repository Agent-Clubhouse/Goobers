package workbenchservice

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/internal/workbenchsuggestions"
)

type suggestionFixtureState struct {
	service  *SuggestionService
	provider *proposalProvider
	g        apiv1.Gaggle
	p        httpapi.Principal
	run      *journal.Run
	reader   *journal.Reader
	value    workbench.RelationshipSuggestion
}

func suggestionServiceFixture(t *testing.T) *suggestionFixtureState {
	t.Helper()
	s, f, g, p, _ := proposalServiceFixture(t)
	g.Spec.Workbench.Sources[0].Paths = []string{"plan.md", "target.md"}
	g.Spec.Workbench.Sources[0].Writes.Relationships = []apiv1.WorkbenchRelationship{"references", "contributes-to"}
	if err := s.ReadService.Permissions.Apply([]apiv1.Gaggle{g}, nil); err != nil {
		t.Fatal(err)
	}
	targetSource := strings.ReplaceAll(documentSource, "obj-00000000-0000-0000-0000-000000000001", "obj-00000000-0000-0000-0000-000000000002")
	entries := []map[string]any{}
	for path, raw := range map[string]string{"plan.md": documentSource, "target.md": targetSource} {
		entries = append(entries, map[string]any{"path": path, "type": "blob", "mode": "100644", "sha": proposalBlob(raw), "size": len(raw)})
		blob, err := json.Marshal(map[string]any{"sha": proposalBlob(raw), "size": len(raw), "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(raw))})
		if err != nil {
			t.Fatal(err)
		}
		f.responses["/repos/acme/code/git/blobs/"+proposalBlob(raw)] = string(blob)
	}
	tree, err := json.Marshal(map[string]any{"sha": strings.Repeat("b", 40), "tree": entries})
	if err != nil {
		t.Fatal(err)
	}
	f.responses["/repos/acme/code/git/trees/"+strings.Repeat("b", 40)] = string(tree)
	set, err := workbench.BindSources(g)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := workbench.SourceTargetDigest(set.Scope, set.Sources[0])
	if err != nil {
		t.Fatal(err)
	}
	endpoint := func(path, raw, objectID string) workbench.SuggestionEndpoint {
		return workbench.SuggestionEndpoint{Ref: &workbench.NodeRef{GaggleID: g.Name, SourceBindingID: "strategy", Kind: "objective-document", SourceID: objectID}, Evidence: &workbench.SuggestionEvidence{SourceTargetDigest: digest, Path: path, RepositoryRevision: &workbench.SuggestionRepositoryRevision{Commit: strings.Repeat("a", 40), BlobID: proposalBlob(raw), ContentDigest: fmt.Sprintf("%x", sha256.Sum256([]byte(raw)))}}}
	}
	value := workbench.RelationshipSuggestion{Kind: "references", Rationale: "The design discusses the same recovery behavior.", From: endpoint("plan.md", documentSource, "obj-00000000-0000-0000-0000-000000000001"), To: endpoint("target.md", targetSource, "obj-00000000-0000-0000-0000-000000000002")}
	root := t.TempDir()
	id := journal.RunIdentity{RunID: strings.Repeat("a", 32), Gaggle: g.Name, Workflow: "curate", WorkflowVersion: 1, ConfigGeneration: strings.Repeat("f", 64)}
	run, err := journal.Create(root, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	if err = run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "curate", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(filepath.Join(root, id.RunID))
	if err != nil {
		t.Fatal(err)
	}
	service := &SuggestionService{Proposals: s, OpenRun: func(_ context.Context, gaggle, runID string) (*journal.Reader, error) {
		if gaggle != id.Gaggle || runID != id.RunID {
			return nil, workbenchsuggestions.ErrArtifact
		}
		return reader, nil
	}}
	return &suggestionFixtureState{service: service, provider: f, g: g, p: p, run: run, reader: reader, value: value}
}
func (f *suggestionFixtureState) emit(t *testing.T) workbench.SuggestionBatch {
	t.Helper()
	raw, err := json.Marshal(workbench.RelationshipSuggestions{SchemaVersion: workbench.SuggestionSchemaVersion, Suggestions: []workbench.RelationshipSuggestion{f.value}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.run.RecordStageArtifact("curate", 1, "", "suggestions.json", raw); err != nil {
		t.Fatal(err)
	}
	inventory, err := f.service.Artifacts(t.Context(), f.p, f.g.Name, strings.Repeat("a", 32), 0)
	if err != nil {
		t.Fatal(err)
	}
	last := inventory.Artifacts[len(inventory.Artifacts)-1]
	batch, err := f.service.Load(t.Context(), f.p, f.g.Name, workbench.SuggestionSelection{RunID: inventory.RunID, Sequence: last.Sequence})
	if err != nil || len(batch.Candidates) != 1 {
		t.Fatal(batch, err)
	}
	return batch
}
func (f *suggestionFixtureState) preview(t *testing.T, batch workbench.SuggestionBatch) workbench.SuggestionDecisionRequest {
	t.Helper()
	preview, err := f.service.Preview(t.Context(), f.p, f.g.Name, workbench.SuggestionPreviewRequest{Selection: batch.Selection, Key: batch.Candidates[0].Suggestion.Key})
	if err != nil || preview.SourceBindingID != "strategy" || !preview.Preview.Changed {
		t.Fatal(preview, err)
	}
	return workbench.SuggestionDecisionRequest{Selection: batch.Selection, Key: batch.Candidates[0].Suggestion.Key, Decision: "accept", Reason: "Reviewed the source references.", ExpectedOwner: &preview.Preview.Expected, ExpectedOperationDigest: preview.Preview.OperationDigest}
}

func TestSuggestionServiceRealArtifactToMetadataPRAndUnknownReplay(t *testing.T) {
	f := suggestionServiceFixture(t)
	batch := f.emit(t)
	request := f.preview(t, batch)
	f.provider.lose = "pulls"
	review, err := f.service.Decide(t.Context(), f.p, f.g.Name, request)
	if err != nil || review.State != "linked" || review.Proposal == nil || review.Proposal.State != "unknown" || f.provider.posts != 4 {
		t.Fatal(review, err, f.provider.posts)
	}
	if review.Suggestion.Origin != batch.Candidates[0].Suggestion.Origin || !strings.Contains(f.provider.content, batch.Candidates[0].Suggestion.Proposal.To.Ref.SourceID) {
		t.Fatal(review, f.provider.content)
	}
	calls := f.provider.gets
	repeat, err := f.service.Decide(t.Context(), f.p, f.g.Name, request)
	if err != nil || !repeat.Duplicate || repeat.ID != review.ID || repeat.Proposal.ID != review.Proposal.ID || f.provider.posts != 4 || f.provider.gets != calls {
		t.Fatal(repeat, err, f.provider.posts, f.provider.gets)
	}
	f.provider.lose = ""
	observed, err := f.service.Proposals.Check(t.Context(), f.p, f.g.Name, "strategy", review.Proposal.ID)
	if err != nil || observed.State != "observed" || f.provider.posts != 4 {
		t.Fatal(observed, err)
	}
	read, err := f.service.Review(t.Context(), f.p, f.g.Name, review.ID)
	if err != nil || read.Proposal.State != "observed" {
		t.Fatal(read, err)
	}
	request.Decision, request.ExpectedOwner, request.ExpectedOperationDigest = "reject", nil, ""
	if _, err = f.service.Decide(t.Context(), f.p, f.g.Name, request); err == nil {
		t.Fatal("accepted decision replaced")
	}
}

func TestSuggestionServiceRefusesChangedIdentityRevisionAndPreviewWithoutEffects(t *testing.T) {
	for _, name := range []string{"identity", "revision", "preview"} {
		t.Run(name, func(t *testing.T) {
			f := suggestionServiceFixture(t)
			if name == "identity" {
				f.value.To.Ref.SourceID = "obj-00000000-0000-0000-0000-000000000003"
			}
			if name == "revision" {
				f.value.To.Evidence.RepositoryRevision.Commit = strings.Repeat("9", 40)
			}
			batch := f.emit(t)
			if name != "preview" {
				if _, err := f.service.Preview(t.Context(), f.p, f.g.Name, workbench.SuggestionPreviewRequest{Selection: batch.Selection, Key: batch.Candidates[0].Suggestion.Key}); err == nil {
					t.Fatal("stale evidence accepted")
				}
			} else {
				request := f.preview(t, batch)
				request.ExpectedOperationDigest = strings.Repeat("0", 64)
				if _, err := f.service.Decide(t.Context(), f.p, f.g.Name, request); err == nil {
					t.Fatal("unreviewed operation accepted")
				}
			}
			if f.provider.posts != 0 {
				t.Fatal("effects before evidence validation")
			}
		})
	}
}

func TestSuggestionServiceCurrentVisibilityAndDecisionGrant(t *testing.T) {
	f := suggestionServiceFixture(t)
	batch := f.emit(t)
	request := workbench.SuggestionDecisionRequest{Selection: batch.Selection, Key: batch.Candidates[0].Suggestion.Key, Decision: "reject", Reason: "Not relevant."}
	changed := f.g.DeepCopy()
	changed.Spec.InteractiveAccess.Actions = []apiv1.InteractiveAction{"repository.read"}
	if err := f.service.Proposals.ReadService.Permissions.Apply([]apiv1.Gaggle{*changed}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Decide(t.Context(), f.p, f.g.Name, request); err == nil {
		t.Fatal("read permission mutated review")
	}
	changed.Spec.InteractiveAccess.Actions = []apiv1.InteractiveAction{"source.proposeChange"}
	if err := f.service.Proposals.ReadService.Permissions.Apply([]apiv1.Gaggle{*changed}, nil); err != nil {
		t.Fatal(err)
	}
	loaded, err := f.service.Load(t.Context(), f.p, f.g.Name, batch.Selection)
	if err != nil || len(loaded.Candidates) != 0 || loaded.Omitted != 1 {
		t.Fatal(loaded, err)
	}
	if f.provider.posts != 0 {
		t.Fatal("read made effects")
	}
}

func TestSuggestionServiceWrongLocatorNeverRebindsStableIssueIdentity(t *testing.T) {
	f := suggestionServiceFixture(t)
	f.g.Spec.Workbench.Sources = append(f.g.Spec.Workbench.Sources, apiv1.WorkbenchSource{Name: "items", Kind: "backlog"})
	f.g.Spec.InteractiveAccess.Actions = append(f.g.Spec.InteractiveAccess.Actions, "backlog.read")
	f.g.Spec.InteractiveAccess.Credentials.Backlog = "human"
	sources := append(repositoryCredentials(), instance.InteractiveCredential{Name: "human", Provider: "github", Owner: "acme", Repository: "issues", Token: instance.TokenRef{Env: "HUMAN_BACKLOG"}})
	permissions, err := interactiveaccess.New([]apiv1.Gaggle{f.g}, sources, interactiveaccess.Dependencies{Registrar: &secretRegistry{}})
	if err != nil {
		t.Fatal(err)
	}
	f.service.Proposals.ReadService.Permissions = permissions
	factory := ProviderFactory{SchedulerDirectory: t.TempDir(), Registrar: &secretRegistry{}, Client: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/repos/acme/issues/issues/43" {
			return issueResponse(r, 200, strings.ReplaceAll(strings.ReplaceAll(issueJSON, "987654", "456789"), `"number":42`, `"number":43`)), nil
		}
		if r.URL.Path == "/repos/acme/issues/issues/42" {
			return issueResponse(r, 200, issueJSON), nil
		}
		return f.provider.roundTrip(r)
	})}}
	f.service.Proposals.ReadService.Backlog = factory.Backlog
	set, err := workbench.BindSources(f.g)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := workbench.SourceTargetDigest(set.Scope, set.Sources[1])
	if err != nil {
		t.Fatal(err)
	}
	f.value.To = workbench.SuggestionEndpoint{Ref: &workbench.NodeRef{GaggleID: f.g.Name, SourceBindingID: "items", Kind: "work-item", SourceID: "987654"}, Evidence: &workbench.SuggestionEvidence{SourceTargetDigest: digest, NativeRevision: "2026-10-04T12:00:00Z", NativeLocator: "43"}}
	batch := f.emit(t)
	if _, err = f.service.Preview(t.Context(), f.p, f.g.Name, workbench.SuggestionPreviewRequest{Selection: batch.Selection, Key: batch.Candidates[0].Suggestion.Key}); err == nil {
		t.Fatal("wrong locator returned different stable ID")
	}
	f.value.To.Evidence.NativeLocator = "42"
	batch = f.emit(t)
	_ = f.preview(t, batch)
	if f.provider.posts != 0 {
		t.Fatal("preview changed provider")
	}
}

func TestSuggestionServicePolicyReloadWaitsForWholeAcceptedEffect(t *testing.T) {
	f := suggestionServiceFixture(t)
	request := f.preview(t, f.emit(t))
	entered, release, published := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.provider.postHook = func(*http.Request) { once.Do(func() { close(entered); <-release }) }
	done := make(chan error, 1)
	go func() { _, err := f.service.Decide(t.Context(), f.p, f.g.Name, request); done <- err }()
	<-entered
	changed := f.g.DeepCopy()
	changed.Spec.InteractiveAccess.Actions = []apiv1.InteractiveAction{"repository.read"}
	reloaded := make(chan error, 1)
	go func() {
		reloaded <- f.service.Proposals.ReadService.Permissions.Apply([]apiv1.Gaggle{*changed}, func() error { close(published); return nil })
	}()
	select {
	case <-published:
		t.Fatal("new policy published during accepted effects")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-reloaded; err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Decide(t.Context(), f.p, f.g.Name, request); err == nil {
		t.Fatal("new acceptance ignored revocation")
	}
}
