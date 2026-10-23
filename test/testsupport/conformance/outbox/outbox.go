// Package outbox is the terminal-report outbox conformance suite. Every
// terminalreport.Outbox implementation runs it, so the cloud outbox keeps the
// publication semantics of the local reference queue.
package outbox

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/terminalreport"
	"github.com/goobers/goobers/providers"
)

// Backend opens views of one durable outbox. Every Open after the first must
// observe what earlier views committed, as a restarted process would. Expire
// makes every deferred or leased intent due, as if its backoff had elapsed on
// the backend's own clock.
type Backend struct {
	Open   func(t *testing.T) terminalreport.Outbox
	Expire func(t *testing.T)
}

// Run executes the suite. newBackend returns an empty outbox per subtest.
func Run(t *testing.T, newBackend func(t *testing.T) Backend) {
	cases := map[string]func(*testing.T, Backend){
		"publish-delivers-once":            publishDeliversOnce,
		"failure-defers-until-due":         failureDefersUntilDue,
		"rate-limit-defers":                rateLimitDefers,
		"newer-intent-replaces-pending":    newerIntentReplacesPending,
		"superseded-is-final":              supersededIsFinal,
		"republish-after-delivery":         republishAfterDelivery,
		"frozen-body-write-did-not-land":   func(t *testing.T, b Backend) { frozenBody(t, b, false) },
		"frozen-body-lost-acknowledgement": func(t *testing.T, b Backend) { frozenBody(t, b, true) },
		"durable-across-reopen":            durableAcrossReopen,
		"reconcile-budget":                 reconcileBudget,
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) { run(t, newBackend(t)) })
	}
}

// Effect returns a work-failure intent for item; count distinguishes intents
// that share the item's key.
func Effect(item string, count int) terminalreport.Effect {
	return terminalreport.Effect{
		Kind:       terminalreport.WorkFailure,
		Repository: providers.RepositoryRef{Provider: providers.ProviderADO, Name: "app"},
		ItemID:     item, RunID: "run-1", Stage: "ci", Count: count,
	}
}

// recorder is an apply function that records each attempt and returns the
// next scripted result.
type recorder struct {
	mu       sync.Mutex
	applied  []terminalreport.Effect
	failures []error
}

func (r *recorder) apply(_ context.Context, e terminalreport.Effect) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.applied = append(r.applied, e)
	if len(r.failures) == 0 {
		return nil
	}
	err := r.failures[0]
	r.failures = r.failures[1:]
	return err
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.applied)
}

func (r *recorder) last() terminalreport.Effect {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.applied[len(r.applied)-1]
}

func reconcile(t *testing.T, outbox terminalreport.Outbox, budget int, r *recorder) {
	t.Helper()
	if err := outbox.Reconcile(t.Context(), budget, r.apply); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

var errUnavailable = errors.New("provider unavailable")

func publishDeliversOnce(t *testing.T, backend Backend) {
	outbox, r := backend.Open(t), &recorder{}
	if err := outbox.Publish(t.Context(), Effect("1", 1), r.apply); err != nil {
		t.Fatal(err)
	}
	if r.count() != 1 || r.last().Count != 1 {
		t.Fatalf("applied %d times, want the intent once", r.count())
	}
	backend.Expire(t)
	reconcile(t, outbox, 10, r)
	if r.count() != 1 {
		t.Fatalf("delivered intent was applied again (%d)", r.count())
	}
}

func failureDefersUntilDue(t *testing.T, backend Backend) {
	outbox, r := backend.Open(t), &recorder{failures: []error{errUnavailable}}
	if err := outbox.Publish(t.Context(), Effect("1", 1), r.apply); !errors.Is(err, errUnavailable) {
		t.Fatalf("failed Publish = %v, want the delivery error", err)
	}
	reconcile(t, outbox, 10, r)
	if r.count() != 1 {
		t.Fatalf("deferred intent retried before its backoff (%d applies)", r.count())
	}
	err := outbox.Publish(t.Context(), Effect("1", 1), r.apply)
	if _, limited := providers.AsRateLimitError(err); !limited || r.count() != 1 {
		t.Fatalf("Publish during backoff = %v after %d applies, want a rate-limit error and no apply", err, r.count())
	}
	backend.Expire(t)
	reconcile(t, outbox, 10, r)
	if r.count() != 2 || r.last().Attempts != 1 {
		t.Fatalf("due intent: %d applies, attempts %d; want a second apply carrying 1 attempt", r.count(), r.last().Attempts)
	}
	backend.Expire(t)
	reconcile(t, outbox, 10, r)
	if r.count() != 2 {
		t.Fatalf("delivered intent was applied again (%d)", r.count())
	}
}

func rateLimitDefers(t *testing.T, backend Backend) {
	limit := &providers.RateLimitError{Provider: providers.ProviderADO, Reset: time.Now().Add(time.Hour)}
	outbox, r := backend.Open(t), &recorder{failures: []error{limit}}
	if err := outbox.Publish(t.Context(), Effect("1", 1), r.apply); err == nil {
		t.Fatal("rate-limited Publish reported success")
	}
	reconcile(t, outbox, 10, r)
	if r.count() != 1 {
		t.Fatalf("rate-limited intent retried before its reset (%d applies)", r.count())
	}
	backend.Expire(t)
	reconcile(t, outbox, 10, r)
	if r.count() != 2 {
		t.Fatalf("due intent applied %d times, want 2", r.count())
	}
}

func newerIntentReplacesPending(t *testing.T, backend Backend) {
	outbox := backend.Open(t)
	failing := &recorder{failures: []error{errUnavailable}}
	if err := outbox.Publish(t.Context(), Effect("1", 1), failing.apply); err == nil {
		t.Fatal("failing Publish reported success")
	}
	backend.Expire(t)
	r := &recorder{}
	if err := outbox.Publish(t.Context(), Effect("1", 2), r.apply); err != nil {
		t.Fatal(err)
	}
	if r.count() != 1 || r.last().Count != 2 {
		t.Fatalf("newer intent: %d applies, want count 2 once", r.count())
	}
	backend.Expire(t)
	reconcile(t, outbox, 10, r)
	if r.count() != 1 {
		t.Fatalf("replaced intent was resurrected (%d applies)", r.count())
	}
}

func supersededIsFinal(t *testing.T, backend Backend) {
	outbox, r := backend.Open(t), &recorder{failures: []error{terminalreport.ErrSuperseded}}
	if err := outbox.Publish(t.Context(), Effect("1", 1), r.apply); err != nil {
		t.Fatalf("superseded Publish = %v, want nil", err)
	}
	backend.Expire(t)
	reconcile(t, outbox, 10, r)
	if r.count() != 1 {
		t.Fatalf("superseded intent was applied again (%d)", r.count())
	}
}

func republishAfterDelivery(t *testing.T, backend Backend) {
	outbox, r := backend.Open(t), &recorder{}
	for range 2 {
		if err := outbox.Publish(t.Context(), Effect("1", 1), r.apply); err != nil {
			t.Fatal(err)
		}
	}
	if r.count() != 2 {
		t.Fatalf("republished intent applied %d times, want 2", r.count())
	}
}

func durableAcrossReopen(t *testing.T, backend Backend) {
	failing := &recorder{failures: []error{errUnavailable}}
	if err := backend.Open(t).Publish(t.Context(), Effect("1", 1), failing.apply); err == nil {
		t.Fatal("failing Publish reported success")
	}
	backend.Expire(t)
	r := &recorder{}
	reconcile(t, backend.Open(t), 10, r)
	if r.count() != 1 || r.last().ItemID != "1" {
		t.Fatalf("reopened outbox applied %d intents, want the pending one", r.count())
	}
}

func reconcileBudget(t *testing.T, backend Backend) {
	outbox := backend.Open(t)
	for _, item := range []string{"1", "2", "3"} {
		failing := &recorder{failures: []error{errUnavailable}}
		if err := outbox.Publish(t.Context(), Effect(item, 1), failing.apply); err == nil {
			t.Fatal("failing Publish reported success")
		}
	}
	backend.Expire(t)
	r := &recorder{}
	reconcile(t, outbox, 2, r)
	if r.count() != 2 {
		t.Fatalf("budget 2 applied %d intents", r.count())
	}
	reconcile(t, outbox, 2, r)
	if r.count() != 3 {
		t.Fatalf("second pass applied %d intents in total, want 3", r.count())
	}
}

// frozenBody checks that a rendered failure body is persisted before the
// provider write and replayed verbatim, and that a write which did land is
// not duplicated after a lost acknowledgement.
func frozenBody(t *testing.T, backend Backend, lostAck bool) {
	outbox := backend.Open(t)
	poster := &poster{readErr: errUnavailable, postErr: errUnavailable}
	apply := func(ctx context.Context, e terminalreport.Effect) error { return e.Apply(ctx, poster) }
	if err := outbox.Publish(t.Context(), Effect("42", 2), apply); err == nil {
		t.Fatal("expected a failed acknowledgement")
	}
	if !lostAck {
		poster.comments = nil
	}
	poster.readErr, poster.postErr = nil, nil
	backend.Expire(t)
	if err := backend.Open(t).Reconcile(t.Context(), 1, apply); err != nil {
		t.Fatal(err)
	}
	switch {
	case lostAck && len(poster.bodies) != 1:
		t.Fatalf("lost acknowledgement posted %d comments, want 1", len(poster.bodies))
	case !lostAck && (len(poster.bodies) != 2 || poster.bodies[0] != poster.bodies[1]):
		t.Fatal("replay changed the frozen body")
	case !strings.Contains(poster.bodies[0], "goobers:terminal-intent"):
		t.Fatal("posted body lacks the intent marker")
	}
	backend.Expire(t)
	r := &recorder{}
	reconcile(t, outbox, 10, r)
	if r.count() != 0 {
		t.Fatal("acknowledged intent remained pending")
	}
}

type poster struct {
	comments []providers.Comment
	readErr  error
	postErr  error
	bodies   []string
}

func (p *poster) GetWorkItem(context.Context, providers.RepositoryRef, string) (providers.WorkItem, error) {
	return providers.WorkItem{}, p.readErr
}

func (p *poster) ListComments(context.Context, providers.RepositoryRef, string) ([]providers.Comment, error) {
	return p.comments, nil
}

func (p *poster) AuthenticatedIdentity(context.Context) (providers.ADOIdentity, error) {
	return providers.ADOIdentity{ID: "bot-guid"}, nil
}

func (p *poster) UpdateComment(context.Context, providers.RepositoryRef, string, string) error {
	return errors.New("editing unsupported")
}

func (p *poster) UpdateWorkItem(_ context.Context, request providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	p.bodies = append(p.bodies, request.Comment)
	p.comments = append(p.comments, providers.Comment{Body: request.Comment, AuthorID: "bot-guid"})
	return providers.WorkItem{}, p.postErr
}
