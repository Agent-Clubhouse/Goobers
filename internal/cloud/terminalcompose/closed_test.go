package terminalcompose

import (
	"context"
	"errors"
	"testing"

	"github.com/goobers/goobers/internal/cloud/terminalwriter"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/terminalhooks/providerwriter"
	"github.com/goobers/goobers/providers"
)

type stateWriter struct {
	providerwriter.Pending
	state string
	err   error
	reads int
}

func (w *stateWriter) WorkItem(context.Context, providerwriter.ItemRef) (providers.WorkItem, error) {
	w.reads++
	return providers.WorkItem{State: w.state}, w.err
}

type stateBackend struct {
	terminalwriter.Backend
	state string
	err   error
}

func (b stateBackend) GetWorkItem(context.Context, providers.RepositoryRef, string) (providers.WorkItem, error) {
	return providers.WorkItem{State: b.state}, b.err
}

type stateBackends struct {
	backend stateBackend
	err     error
}

func (b stateBackends) For(context.Context, terminalwriter.Binding, providers.RepositoryRef, string) (terminalwriter.Backend, error) {
	return b.backend, b.err
}

var repo = providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}

// Only a definite non-open state is "closed": an item whose state is empty or
// unreadable must keep its record, or a transient read would erase a real block.
func TestClosedViaTreatsOnlyADefiniteNonOpenStateAsClosed(t *testing.T) {
	for name, tc := range map[string]struct {
		state  string
		err    error
		closed bool
		fails  bool
	}{
		"open":         {state: "open"},
		"open (cased)": {state: "OPEN"},
		"closed":       {state: "closed", closed: true},
		"completed":    {state: "completed", closed: true},
		"empty state":  {state: ""},
		"read failure": {err: errors.New("provider unavailable"), fails: true},
	} {
		t.Run(name, func(t *testing.T) {
			w := &stateWriter{state: tc.state, err: tc.err}
			closed, err := closedVia(Deps{Writer: w})(t.Context(), "run-1", repo, "7")
			if closed != tc.closed || (err != nil) != tc.fails {
				t.Fatalf("closed %v, err %v; want closed %v, fails %v", closed, err, tc.closed, tc.fails)
			}
		})
	}
}

// The instance-level binding is tried first and needs no run; the recording run
// is only the fallback, for a repository the applied configuration does not own.
func TestClosedViaPrefersTheInstanceBindingAndFallsBackToTheRun(t *testing.T) {
	scrub := journal.NewRegistryScrubber()
	scrub.Register([]byte("tok-123"))
	w := &stateWriter{state: "open"}
	deps := Deps{
		Writer:   w,
		Backends: stateBackends{backend: stateBackend{state: "closed"}},
		Instance: func(providers.RepositoryRef) (terminalwriter.Binding, bool) {
			return terminalwriter.Binding{Gaggle: "web"}, true
		},
		Scrubber: scrub,
	}
	if closed, err := closedVia(deps)(t.Context(), "swept-run", repo, "7"); err != nil || !closed || w.reads != 0 {
		t.Fatalf("via instance: closed %v, err %v, run reads %d; want closed through the instance binding alone", closed, err, w.reads)
	}
	// No gaggle owns the repository: fall back to the recording run.
	deps.Instance = func(providers.RepositoryRef) (terminalwriter.Binding, bool) { return terminalwriter.Binding{}, false }
	if closed, err := closedVia(deps)(t.Context(), "run-1", repo, "7"); err != nil || closed || w.reads != 1 {
		t.Fatalf("fallback: closed %v, err %v, run reads %d; want one read through the run", closed, err, w.reads)
	}
	// An instance-level failure is reported scrubbed and does NOT fall back to
	// the run: it would only mask a broken credential with a possibly swept pin.
	deps.Instance = func(providers.RepositoryRef) (terminalwriter.Binding, bool) {
		return terminalwriter.Binding{Gaggle: "web"}, true
	}
	deps.Backends = stateBackends{backend: stateBackend{err: errors.New("401 with token tok-123")}}
	_, err := closedVia(deps)(t.Context(), "run-1", repo, "7")
	if err == nil || containsSecret(err.Error(), "tok-123") {
		t.Fatalf("instance read failure = %v, want an error with the credential redacted", err)
	}
}

func containsSecret(text, secret string) bool {
	for i := 0; i+len(secret) <= len(text); i++ {
		if text[i:i+len(secret)] == secret {
			return true
		}
	}
	return false
}
