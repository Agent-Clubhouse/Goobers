package workbenchservice

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/providers"
)

type resolutionProvider struct {
	t                                        *testing.T
	kind                                     apiv1.Provider
	mu                                       sync.Mutex
	reads, mutations, version                int
	marker, lost, incomplete, openDependency bool
	held                                     atomic.Bool
	hook                                     func(*http.Request)
}

func (f *resolutionProvider) roundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.held.Load() || r.Header.Get("Authorization") == "" || strings.Contains(r.Header.Get("Authorization"), "automation") {
		f.t.Fatal("provider effect outside held interactive/claims scope")
	}
	if strings.HasSuffix(r.URL.Path, "/states") {
		return issueResponse(r, 200, `{"value":[{"name":"Active","category":"InProgress"},{"name":"Closed","category":"Completed"}]}`), nil
	}
	if strings.HasSuffix(r.URL.Path, "/comments") {
		body := `[]`
		if f.kind == "ado" {
			body = `{"comments":[]}`
		}
		response := issueResponse(r, 200, body)
		if f.incomplete {
			if f.kind == "ado" {
				response.Header.Set("x-ms-continuationtoken", "more")
			} else {
				response.Header.Set("Link", `<https://api.github.com/next>; rel="next"`)
			}
		}
		return response, nil
	}
	if strings.HasSuffix(r.URL.Path, "/blocked_by") {
		return issueResponse(r, 200, `[]`), nil
	}
	if r.Method == http.MethodDelete || r.Method == http.MethodPatch {
		f.mutations++
		if f.kind == "github" {
			if !strings.HasSuffix(r.URL.Path, "/labels/"+providers.LabelNeedsHuman) {
				f.t.Fatal("broader marker effect", r.URL)
			}
		} else {
			var patch []struct {
				Op, Path string
				Value    json.RawMessage
			}
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				f.t.Fatal(err)
			}
			if len(patch) != 2 || patch[0].Op != "test" || patch[0].Path != "/rev" || string(patch[0].Value) != fmt.Sprint(f.version) || patch[1].Path != "/fields/System.Tags" || string(patch[1].Value) != `"keep"` {
				f.t.Fatal("broader or unconditioned tags effect", patch)
			}
		}
		f.marker = false
		f.version++
		if f.hook != nil {
			f.hook(r)
		}
		if f.lost {
			return issueResponse(r, 503, `{"message":"private-provider-error"}`), nil
		}
		if f.kind == "github" {
			return issueResponse(r, 204, ``), nil
		}
	} else {
		f.reads++
	}
	id := "42"
	state := "open"
	nativeState := "Active"
	stable := "987654"
	if strings.HasSuffix(r.URL.Path, "/43") {
		id = "43"
		stable = "987655"
		if !f.openDependency {
			state = "closed"
			nativeState = "Closed"
		}
	}
	labels := `[{"name":"keep"}]`
	tags := "keep"
	if f.marker && id == "42" {
		labels = `[{"name":"keep"},{"name":"` + providers.LabelNeedsHuman + `"}]`
		tags += "; " + strings.ToUpper(providers.LabelNeedsHuman)
	}
	if f.kind == "github" {
		return issueResponse(r, 200, fmt.Sprintf(`{"id":%s,"number":%s,"title":"Waiting","state":%q,"html_url":"https://github.com/acme/issues/issues/%s","updated_at":"2026-10-04T12:00:%02dZ","labels":%s,"assignees":[]}`, stable, id, state, id, f.version, labels)), nil
	}
	return issueResponse(r, 200, fmt.Sprintf(`{"id":%s,"rev":%d,"url":"https://dev.azure.com/acme/_apis/wit/workItems/%s","fields":{"System.TeamProject":"issues","System.Title":"Waiting","System.State":%q,"System.WorkItemType":"Feature","System.Tags":%q},"relations":[]}`, id, f.version, id, nativeState, tags)), nil
}
func resolutionFixture(t *testing.T, kind apiv1.Provider) (*SessionResolver, *resolutionProvider, *LearnedBlock) {
	t.Helper()
	writer, _, g, p, _ := writerFixture(t, kind)
	g.Spec.InteractiveAccess.Actions = append(g.Spec.InteractiveAccess.Actions, "backlog.resolve")
	g.Spec.Workbench.Sources[0].Writes.Fields = append(g.Spec.Workbench.Sources[0].Writes.Fields, "labels")
	if err := writer.ReadService.Permissions.Apply([]apiv1.Gaggle{g}, nil); err != nil {
		t.Fatal(err)
	}
	f := &resolutionProvider{t: t, kind: kind, marker: true, version: 1}
	writer.ReadService.Backlog = ProviderFactory{SchedulerDirectory: t.TempDir(), Client: &http.Client{Transport: roundTrip(f.roundTrip)}}.Backlog
	actor := sessioning.Actor{Issuer: p.Issuer, Subject: p.Subject}
	id := resolutionTurn(t, writer.Queue, g.Name, actor)
	lease, err := writer.ReadService.Permissions.BeginSessionExecution(t.Context(), p, g.Name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lease.Close)
	learned := &LearnedBlock{Digest: strings.Repeat("b", 64), Complete: true}
	var claims sync.Mutex
	service := &ResolutionService{ReadService: writer.ReadService, Queue: writer.Queue, WithLearnedBlock: func(ctx context.Context, _ providers.RepositoryRef, _ string, use func(context.Context, LearnedBlock) error) error {
		claims.Lock()
		defer claims.Unlock()
		f.held.Store(true)
		defer f.held.Store(false)
		return use(ctx, *learned)
	}}
	resolver, err := service.ForSession(t.Context(), &g, lease, actor, id)
	if err != nil {
		t.Fatal(err)
	}
	return resolver, f, learned
}
func resolutionTurn(t *testing.T, queue *triggerqueue.Store, gaggle string, actor sessioning.Actor) journal.RunIdentity {
	t.Helper()
	now := time.Now().UTC()
	digest := sessioning.Digest([]byte("pinned profile"))
	command := func(key string) triggerqueue.SessionCommand {
		return triggerqueue.SessionCommand{Gaggle: gaggle, Actor: actor, RequestID: key, RequestDigest: sessioning.Digest([]byte(key))}
	}
	created, err := queue.CreateSession(t.Context(), command("create"), "Resolve blocker", sessioning.Profile{Goober: "planner", ConfigGeneration: digest, GooberDigest: digest}, now)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := queue.SubmitSessionMessage(t.Context(), command("instruction"), created.Session.ID, "The requested design decision is yes. Resolve this blocker if the current evidence supports it.", []byte(`{"verified":true}`), now)
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := queue.SessionInputs(t.Context(), accepted.AcceptanceID)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := queue.BeginSessionTurn(t.Context(), accepted.AcceptanceID, now)
	if err != nil {
		t.Fatal(err)
	}
	runID := strings.TrimPrefix(accepted.AcceptanceID, "trigger-")
	if err = queue.ObserveSessionRun(t.Context(), accepted.AcceptanceID, runID, now); err != nil {
		t.Fatal(err)
	}
	raw, err := inputs.Validate(runID, gaggle)
	if err != nil {
		t.Fatal(err)
	}
	id := journal.RunIdentity{InstanceID: "instance", RunID: runID, Gaggle: gaggle, Workflow: "interactive-session", ConfigGeneration: digest, WorkflowDigest: digest, GooberDigest: digest, Trigger: journal.Trigger{Kind: journal.TriggerSignal, Ref: "session:" + turn.Session.ID + ":" + turn.ID}, Session: &journal.SessionLineage{Gaggle: gaggle, SessionID: turn.Session.ID, TurnID: turn.ID, MessageID: turn.Message.ID, AcceptanceID: accepted.AcceptanceID, EnvelopeDigest: sessioning.Digest(turn.Record.Payload), InputDigest: sessioning.Digest(raw)}}
	run, err := journal.Create(t.TempDir(), id, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	return id
}
func inspectResolution(t *testing.T, s *SessionResolver) workbench.NeedsHumanObservation {
	t.Helper()
	stable := "987654"
	if s.retained.Spec.Backlog.Provider == "ado" {
		stable = "42"
	}
	observation, err := s.Inspect(t.Context(), "items", workbench.BacklogItemRequest{ID: "42", ExpectedSourceID: stable})
	if err != nil {
		t.Fatal(err)
	}
	return observation
}
func assessedResolution(o workbench.NeedsHumanObservation) workbench.NeedsHumanResolutionRequest {
	return workbench.NeedsHumanResolutionRequest{ID: o.Item.Locator.ID, SourceID: o.Item.Ref.SourceID, ExpectedRevision: o.Item.Revision, ObservationDigest: o.Digest, Basis: *o.HumanInstruction, Rationale: "The current human decision resolves the question; all known dependencies are verified closed.", Evidence: []workbench.NeedsHumanEvidenceRef{*o.HumanInstruction}}
}
func TestNeedsHumanResolutionNativeCustodyAndLostReply(t *testing.T) {
	for _, kind := range []apiv1.Provider{"github", "ado"} {
		for _, lost := range []bool{false, true} {
			t.Run(fmt.Sprint(kind, lost), func(t *testing.T) {
				s, f, _ := resolutionFixture(t, kind)
				f.lost = lost
				request := assessedResolution(inspectResolution(t, s))
				result, err := s.Resolve(t.Context(), "items", "turn-resolve", request)
				expected := "confirmed"
				if lost {
					expected = "unknown"
				}
				if err != nil || result.State != expected || result.Receipt == nil || result.Receipt.ProviderAcknowledged == lost || !result.Receipt.ObservedClear || f.mutations != 1 {
					t.Fatal(result, err, f.mutations)
				}
				reads := f.reads
				t.Setenv("HUMAN_BACKLOG", "")
				replay, err := s.Resolve(t.Context(), "items", "turn-resolve", request)
				if err != nil || !replay.Duplicate || replay.ID != result.ID || replay.State != expected || f.mutations != 1 || f.reads != reads {
					t.Fatal(replay, err)
				}
				receipt, err := s.Command(t.Context(), "items", result.ID)
				if err != nil || receipt.ID != result.ID || f.reads != reads {
					t.Fatal(receipt, err)
				}
			})
		}
	}
}
func TestNeedsHumanResolutionRefusesIncompleteChangedOrFabricatedEvidence(t *testing.T) {
	for _, mode := range []string{"comments", "learned-open", "learned-unknown", "record-changed", "fake-human", "fake-comment", "wrong-actor", "input-custody"} {
		t.Run(mode, func(t *testing.T) {
			s, f, learned := resolutionFixture(t, "github")
			if mode == "comments" {
				f.incomplete = true
			}
			if mode == "learned-open" {
				learned.Blockers = []string{"43"}
				f.openDependency = true
			}
			if mode == "learned-unknown" {
				learned.Complete = false
			}
			observation := inspectResolution(t, s)
			request := assessedResolution(observation)
			switch mode {
			case "record-changed":
				learned.Digest = strings.Repeat("c", 64)
			case "fake-human":
				request.Basis.ID = "fabricated"
			case "fake-comment":
				request.Evidence = []workbench.NeedsHumanEvidenceRef{{Kind: "source-comment", ID: "made-up", Digest: strings.Repeat("c", 64)}}
			case "wrong-actor":
				s.actor.Subject = "other"
			case "input-custody":
				s.identity.Session.InputDigest = sessioning.Digest([]byte("other"))
			}
			if _, err := s.Resolve(t.Context(), "items", "refused", request); err == nil || f.mutations != 0 {
				t.Fatal("unsafe resolution", err, f.mutations)
			}
		})
	}
}
func TestNeedsHumanResolutionRevocationJoinsUnknownReceipt(t *testing.T) {
	s, f, _ := resolutionFixture(t, "github")
	request := assessedResolution(inspectResolution(t, s))
	f.lost = true
	entered := make(chan struct{})
	f.hook = func(r *http.Request) { close(entered); <-r.Context().Done() }
	done := make(chan workbench.NeedsHumanResolutionCommand, 1)
	errs := make(chan error, 1)
	go func() {
		result, err := s.Resolve(t.Context(), "items", "cancelled", request)
		s.lease.Close()
		done <- result
		errs <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("effect did not start")
	}
	changed := s.retained.DeepCopy()
	changed.Spec.InteractiveAccess.Actions = nil
	applied := make(chan error, 1)
	go func() { applied <- s.service.ReadService.Permissions.Apply([]apiv1.Gaggle{*changed}, nil) }()
	select {
	case result := <-done:
		if err := <-errs; err != nil || result.State != "unknown" {
			t.Fatal(result, err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("policy reload deadlock")
	}
	select {
	case err := <-applied:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("policy did not join")
	}
	record, err := s.service.Queue.FindNeedsHumanCommand(t.Context(), s.scope("items"), "cancelled")
	if err != nil || record.State != "unknown" || record.Receipt == nil || f.mutations != 1 {
		t.Fatal(record, err)
	}
}

func TestNeedsHumanResolutionConcurrentDuplicateClaimsOnlyOneNativeEffect(t *testing.T) {
	s, f, learned := resolutionFixture(t, "github")
	learned.Reason = "The recorded design question has now been answered."
	learned.Blockers = []string{"43"}
	observation := inspectResolution(t, s)
	request := assessedResolution(observation)
	request.Basis = workbench.NeedsHumanEvidenceRef{Kind: "learned-record", ID: "42", Digest: learned.Digest}
	entered := make(chan struct{})
	release := make(chan struct{})
	f.hook = func(*http.Request) { close(entered); <-release }
	results := make(chan workbench.NeedsHumanResolutionCommand, 2)
	errs := make(chan error, 2)
	go func() { r, e := s.Resolve(t.Context(), "items", "shared-key", request); results <- r; errs <- e }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first attempt absent")
	}
	go func() { r, e := s.Resolve(t.Context(), "items", "shared-key", request); results <- r; errs <- e }()
	close(release)
	first, second := <-results, <-results
	if e := <-errs; e != nil {
		t.Fatal(e)
	}
	if e := <-errs; e != nil {
		t.Fatal(e)
	}
	if first.ID == "" || first.ID != second.ID || f.mutations != 1 {
		t.Fatal(first, second, f.mutations)
	}
	receipt, err := s.Command(t.Context(), "items", first.ID)
	if err != nil || receipt.State != "confirmed" {
		t.Fatal(receipt, err)
	}
}
