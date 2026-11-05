package terminalwriter

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/cloud/lease"
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
	// journalErr fails the next appends (a failed or fenced instance log).
	journalErr error
}

func newInstanceRig(t *testing.T) *instanceRig {
	t.Helper()
	r := &instanceRig{backend: &fakeBackend{kind: providers.ProviderGitHub, calls: &[]call{}, labels: map[string][]string{}}, applied: true}
	_, scrubber := journal.DefaultScrubber()
	w, err := NewInstanceWriter(func() (*instance.ConfigSet, string, bool) { return escalationSet(), "digest-1", r.applied },
		fakeBackends{r.backend}, scrubber, func(ev journal.Event) error {
			if r.journalErr != nil {
				return r.journalErr
			}
			r.events = append(r.events, ev)
			return nil
		})
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
	w, err := NewInstanceWriter(func() (*instance.ConfigSet, string, bool) { return escalationSet(), "d", true }, fakeBackends{r.backend}, scrubber, func(journal.Event) error { return nil })
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
	if _, err := NewInstanceWriter(nil, fakeBackends{}, scrubber, func(journal.Event) error { return nil }); err == nil {
		t.Fatal("no applied configuration accepted")
	}
	if _, err := NewInstanceWriter(applied, nil, scrubber, func(journal.Event) error { return nil }); err == nil {
		t.Fatal("no backends accepted")
	}
	if _, err := NewInstanceWriter(applied, fakeBackends{}, nil, func(journal.Event) error { return nil }); err == nil {
		t.Fatal("no scrubber accepted")
	}
	if _, err := NewInstanceWriter(applied, fakeBackends{}, scrubber, nil); err == nil {
		t.Fatal("no journal accepted: an unjournaled write path must not exist")
	}
}

// A write whose journal entry cannot be appended is not made. The journal is the
// epoch's fenced log, so this is also what stops a deposed leader's credentialed
// write: its append fails with ErrStale before the provider is touched.
func TestInstanceWriterRefusesAWriteItCannotJournal(t *testing.T) {
	r := newInstanceRig(t)
	r.journalErr = errors.New("instance log unavailable")
	if _, err := r.writer.UpdateWorkItem(t.Context(), escalation()); err == nil || !strings.Contains(err.Error(), "journal the escalation write") {
		t.Fatalf("err = %v, want the write refused for want of a journal entry", err)
	}
	if len(*r.backend.calls) != 0 {
		t.Fatalf("the provider was written despite a failed journal append: %+v", *r.backend.calls)
	}
	// A deposed leader's fenced append fails with ErrStale, which is passed through.
	r.journalErr = lease.ErrStale
	_, err := r.writer.UpdateWorkItem(t.Context(), escalation())
	if !errors.Is(err, lease.ErrStale) || len(*r.backend.calls) != 0 {
		t.Fatalf("a fenced leader's write: err %v, calls %+v; want ErrStale and no provider call", err, *r.backend.calls)
	}
	// And a journal failure is not permanent: the sweep retries it.
	if providerwriter.Permanent(err) {
		t.Fatal("a journal failure was classified permanent")
	}
}

// Every field of UpdateWorkItemRequest is either allowed by the escalation
// allowlist or refused. A field added to the provider request later fails this
// test until the allowlist decides it, so a new capability cannot slip into the one
// credentialed write path unnoticed.
func TestInstanceWriterDecidesEveryUpdateWorkItemRequestField(t *testing.T) {
	decided := map[string]string{
		"Repository":       "required: must be a configured gaggle's backlog repository",
		"ID":               "required",
		"AddLabels":        "allowlist: needs-human only",
		"RemoveLabels":     "allowlist: ready and claimed only",
		"Comment":          "allowed with an idempotency key",
		"IdempotencyKey":   "required with a comment",
		"ExpectedRevision": "refused",
		"Title":            "refused",
		"Body":             "refused",
		"Assignee":         "refused",
		"Milestone":        "refused",
		"State":            "refused",
	}
	typ := reflect.TypeOf(providers.UpdateWorkItemRequest{})
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		if _, ok := decided[name]; !ok {
			t.Errorf("UpdateWorkItemRequest.%s is new: decide it in validateEscalation and list it here", name)
		}
	}
	for name := range decided {
		if _, ok := typ.FieldByName(name); !ok {
			t.Errorf("%s is listed but no longer a field", name)
		}
	}
	// The refused fields really are refused: set each in turn on a valid request.
	str, num := "x", 1
	for name, set := range map[string]func(*providers.UpdateWorkItemRequest){
		"ExpectedRevision": func(r *providers.UpdateWorkItemRequest) { r.ExpectedRevision = "r" },
		"Title":            func(r *providers.UpdateWorkItemRequest) { r.Title = &str },
		"Body":             func(r *providers.UpdateWorkItemRequest) { r.Body = &str },
		"Assignee":         func(r *providers.UpdateWorkItemRequest) { r.Assignee = &str },
		"Milestone":        func(r *providers.UpdateWorkItemRequest) { r.Milestone = &num },
		"State":            func(r *providers.UpdateWorkItemRequest) { r.State = "closed" },
	} {
		req := escalation()
		set(&req)
		if validateEscalation(req) == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
