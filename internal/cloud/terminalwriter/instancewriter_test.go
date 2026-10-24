package terminalwriter

import (
	"errors"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/terminalhooks/providerwriter"
	"github.com/goobers/goobers/providers"
)

func escalationSet() *instance.ConfigSet {
	g := apiv1.Gaggle{}
	g.Name = "web"
	g.Spec.Project = apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "backlog"}
	return &instance.ConfigSet{Gaggles: []apiv1.Gaggle{g}}
}

type instanceRig struct {
	writer  *InstanceWriter
	backend *fakeBackend
	events  []journal.Event
	applied bool
}

func newInstanceRig(t *testing.T) *instanceRig {
	t.Helper()
	r := &instanceRig{backend: &fakeBackend{kind: providers.ProviderGitHub, calls: &[]call{}, labels: map[string][]string{}}, applied: true}
	_, scrubber := journal.DefaultScrubber()
	w, err := NewInstanceWriter(func() (*instance.ConfigSet, string, bool) { return escalationSet(), "digest-1", r.applied },
		fakeBackends{r.backend}, scrubber, func(ev journal.Event) { r.events = append(r.events, ev) })
	if err != nil {
		t.Fatal(err)
	}
	r.writer = w
	return r
}

func escalation() providers.UpdateWorkItemRequest {
	return providers.UpdateWorkItemRequest{
		Repository: backlogRepo, ID: "7", Comment: "cycle report", IdempotencyKey: "cycle-abc/7/0",
		AddLabels: []string{providers.LabelNeedsHuman}, RemoveLabels: []string{providers.LabelReady, providers.LabelClaimed},
	}
}

func TestInstanceWriterMakesTheEscalationWriteWithIssuesWriteAndJournalsItFirst(t *testing.T) {
	r := newInstanceRig(t)
	if _, err := r.writer.UpdateWorkItem(t.Context(), escalation()); err != nil {
		t.Fatal(err)
	}
	if len(*r.backend.calls) != 1 || (*r.backend.calls)[0].op != "update" || (*r.backend.calls)[0].capability != string(capability.GitHubIssuesWrite) || (*r.backend.calls)[0].repo != backlogRepo {
		t.Fatalf("calls = %+v, want one issues:write update on the backlog repository", *r.backend.calls)
	}
	if len(r.events) != 1 || r.events[0].Runner["kind"] != InstanceWriteOperation || r.events[0].Runner["itemId"] != "7" || r.events[0].Runner["idempotencyKey"] != "cycle-abc/7/0" {
		t.Fatalf("journal = %+v, want the write journaled with its cycle key", r.events)
	}
}

// Every refusal is permanent, reaches no provider, and journals nothing.
func TestInstanceWriterRefusesAnythingButAnEscalation(t *testing.T) {
	str := func(s string) *string { return &s }
	num := 3
	cases := map[string]func(*providers.UpdateWorkItemRequest){
		"a repository no gaggle owns": func(r *providers.UpdateWorkItemRequest) { r.Repository = otherRepo },
		"the workspace repository":    func(r *providers.UpdateWorkItemRequest) { r.Repository = workspaceRepo },
		"an unkeyed comment":          func(r *providers.UpdateWorkItemRequest) { r.IdempotencyKey = "" },
		"closing an issue":            func(r *providers.UpdateWorkItemRequest) { r.State = "closed" },
		"opening an issue":            func(r *providers.UpdateWorkItemRequest) { r.State = "open" },
		"editing the title":           func(r *providers.UpdateWorkItemRequest) { r.Title = str("x") },
		"editing the body":            func(r *providers.UpdateWorkItemRequest) { r.Body = str("x") },
		"assigning":                   func(r *providers.UpdateWorkItemRequest) { r.Assignee = str("someone") },
		"setting a milestone":         func(r *providers.UpdateWorkItemRequest) { r.Milestone = &num },
		"an expected revision":        func(r *providers.UpdateWorkItemRequest) { r.ExpectedRevision = "r1" },
		"adding ready":                func(r *providers.UpdateWorkItemRequest) { r.AddLabels = []string{providers.LabelReady} },
		"adding merge-ready":          func(r *providers.UpdateWorkItemRequest) { r.AddLabels = []string{"goobers:merge-ready"} },
		"removing needs-human":        func(r *providers.UpdateWorkItemRequest) { r.RemoveLabels = []string{providers.LabelNeedsHuman} },
		"removing an unrelated label": func(r *providers.UpdateWorkItemRequest) { r.RemoveLabels = []string{"bug"} },
		"no item":                     func(r *providers.UpdateWorkItemRequest) { r.ID = "" },
		"an empty request": func(r *providers.UpdateWorkItemRequest) {
			*r = providers.UpdateWorkItemRequest{Repository: backlogRepo, ID: "7"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := newInstanceRig(t)
			req := escalation()
			mutate(&req)
			_, err := r.writer.UpdateWorkItem(t.Context(), req)
			if err == nil || !providerwriter.Permanent(err) {
				t.Fatalf("err = %v, want a permanent refusal", err)
			}
			if len(*r.backend.calls) != 0 || len(r.events) != 0 {
				t.Fatalf("a refused write reached the provider %+v or the journal %+v", *r.backend.calls, r.events)
			}
		})
	}
}

func TestInstanceWriterFailsClosedWithoutAnAppliedConfigAndOnAnUnsupportedComment(t *testing.T) {
	r := newInstanceRig(t)
	r.applied = false
	if _, err := r.writer.UpdateWorkItem(t.Context(), escalation()); !errors.Is(err, ErrNoAppliedConfig) || providerwriter.Permanent(err) {
		t.Fatalf("before any config is applied: %v, want the transient ErrNoAppliedConfig", err)
	}
	// Azure Boards cannot dedupe a retried comment.
	r = newInstanceRig(t)
	r.backend.kind = providers.ProviderADO
	if _, err := r.writer.UpdateWorkItem(t.Context(), escalation()); err == nil || !providerwriter.Permanent(err) || len(*r.backend.calls) != 0 {
		t.Fatalf("an ADO comment: %v (calls %+v), want a permanent refusal before any write", err, *r.backend.calls)
	}
	// Labels alone are fine there.
	labels := escalation()
	labels.Comment, labels.IdempotencyKey = "", ""
	if _, err := r.writer.UpdateWorkItem(t.Context(), labels); err != nil {
		t.Fatalf("labels-only on ADO: %v", err)
	}
}

func TestInstanceWriterScrubsProviderFailuresAndKeepsTheirKind(t *testing.T) {
	r := newInstanceRig(t)
	registry, scrubber := journal.DefaultScrubber()
	registry.Register([]byte("tok-123"))
	r.backend.err = &providers.RateLimitError{Provider: providers.ProviderGitHub, Endpoint: "https://x/tok-123"}
	w, err := NewInstanceWriter(func() (*instance.ConfigSet, string, bool) { return escalationSet(), "d", true }, fakeBackends{r.backend}, scrubber, func(journal.Event) {})
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.UpdateWorkItem(t.Context(), escalation())
	if err == nil || strings.Contains(err.Error(), "tok-123") {
		t.Fatalf("err = %v, want a scrubbed failure", err)
	}
	if _, limited := providers.AsRateLimitError(err); !limited {
		t.Fatalf("err = %v lost its rate-limit kind", err)
	}
}

func TestNewInstanceWriterRequiresEverything(t *testing.T) {
	_, scrubber := journal.DefaultScrubber()
	applied := func() (*instance.ConfigSet, string, bool) { return nil, "", false }
	if _, err := NewInstanceWriter(nil, fakeBackends{}, scrubber, func(journal.Event) {}); err == nil {
		t.Fatal("no applied configuration accepted")
	}
	if _, err := NewInstanceWriter(applied, nil, scrubber, func(journal.Event) {}); err == nil {
		t.Fatal("no backends accepted")
	}
	if _, err := NewInstanceWriter(applied, fakeBackends{}, nil, func(journal.Event) {}); err == nil {
		t.Fatal("no scrubber accepted")
	}
	if _, err := NewInstanceWriter(applied, fakeBackends{}, scrubber, nil); err == nil {
		t.Fatal("no journal accepted: an unjournaled write path must not exist")
	}
}
