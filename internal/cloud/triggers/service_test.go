package triggers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type fakeStore struct {
	mu      sync.Mutex
	records map[string]triggerqueue.Record
	byKey   map[string]string
	accepts int
}

// Accept compares payload BYTES, exactly like the PG store, so any envelope
// field that varies between attempts surfaces as a conflict here too.
func (s *fakeStore) Accept(_ context.Context, key, actor string, payload []byte, now time.Time) (triggerqueue.Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accepts++
	if s.records == nil {
		s.records, s.byKey = map[string]triggerqueue.Record{}, map[string]string{}
	}
	if id, ok := s.byKey[key]; ok {
		prior := s.records[id]
		if prior.Actor != actor || string(prior.Payload) != string(payload) {
			return triggerqueue.Record{}, false, triggerqueue.ErrConflict
		}
		return prior, true, nil
	}
	record := triggerqueue.Record{ID: "trigger-" + key, Key: key, Actor: actor, Payload: payload, State: triggerqueue.Accepted, AcceptedAt: now}
	s.records[record.ID], s.byKey[key] = record, record.ID
	return record, false, nil
}

func (s *fakeStore) ByKey(_ context.Context, key string) (triggerqueue.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.byKey[key]; ok {
		return s.records[id], nil
	}
	return triggerqueue.Record{}, sql.ErrNoRows
}

func (s *fakeStore) Get(_ context.Context, id, actor string) (triggerqueue.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.records[id]; ok && r.Actor == actor {
		return r, nil
	}
	return triggerqueue.Record{}, sql.ErrNoRows
}

// fakeAdmission records every consultation so a matrix can assert "not consulted".
type fakeAdmission struct {
	mu         sync.Mutex
	generation string
	refuse     map[string]bool
	calls      int
}

func (f *fakeAdmission) AdmitApplied(_, workflow string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.generation == "" {
		return "", fmt.Errorf("%w: %w", cloudconfig.ErrIneligible, cloudconfig.ErrNoAppliedGeneration)
	}
	if f.refuse[workflow] {
		return "", fmt.Errorf("%w: refused", cloudconfig.ErrIneligible)
	}
	return f.generation, nil
}

func (f *fakeAdmission) setGeneration(generation string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.generation = generation
}

type recordingPin struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (p *recordingPin) pin(_ context.Context, generation, _, _ string) (string, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.err != nil {
		return "", "", p.err
	}
	return "wf-" + generation, "goober-" + generation, nil
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Second)
	return c.t
}

func newService(store Store, admission Admission) (*Service, *recordingPin) {
	pin := &recordingPin{}
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	return &Service{
		Store:     store,
		Admission: func() Admission { return admission },
		Pin:       pin.pin,
		Now:       c.now, // advances on every call, like a real clock
	}, pin
}

func status(err error) int {
	var intervention *httpapi.InterventionError
	if errors.As(err, &intervention) {
		return intervention.Status
	}
	return 0
}

func request() httpapi.TriggerRequest {
	return httpapi.TriggerRequest{Gaggle: "g", Workflow: "review", RequestID: "key-1", Actor: "operator"}
}

func TestTriggerAcceptsPinnedEnvelope(t *testing.T) {
	store := &fakeStore{}
	service, _ := newService(store, &fakeAdmission{generation: "gen-1"})
	response, err := service.Trigger(t.Context(), request())
	if err != nil || response.Duplicate || response.State != string(triggerqueue.Accepted) {
		t.Fatalf("response = %+v, %v", response, err)
	}
	envelope, err := startintent.Parse(store.records[response.AcceptanceID].Payload)
	if err != nil {
		t.Fatal(err)
	}
	want := startintent.Target{Gaggle: "g", Workflow: "review", ConfigGeneration: "gen-1", WorkflowDigest: "wf-gen-1", GooberDigest: "goober-gen-1"}
	if envelope.Target != want {
		t.Fatalf("target = %+v, want %+v", envelope.Target, want)
	}
}

func TestRetryWithAdvancingClockReplays(t *testing.T) {
	store := &fakeStore{}
	service, _ := newService(store, &fakeAdmission{generation: "gen-1"})
	first, err := service.Trigger(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	again, err := service.Trigger(t.Context(), request())
	if err != nil || !again.Duplicate || again.AcceptanceID != first.AcceptanceID {
		t.Fatalf("retry = %+v, %v; want the original acceptance", again, err)
	}
}

func TestRetryAfterConfigurationChangesNeverRefuses(t *testing.T) {
	store := &fakeStore{}
	admission := &fakeAdmission{generation: "gen-1"}
	service, pin := newService(store, admission)
	first, err := service.Trigger(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	before := string(store.records[first.AcceptanceID].Payload)
	pinCalls, admitCalls := pin.calls, admission.calls

	// A newer generation is applied, then the workflow becomes ineligible, then
	// the plane loses its configuration and pinning altogether.
	admission.setGeneration("gen-2")
	admission.refuse = map[string]bool{"review": true}
	pin.err = ErrPinUnavailable
	for name, mutate := range map[string]func(){
		"new generation":      func() { admission.refuse = nil; pin.err = nil },
		"now ineligible":      func() { admission.refuse = map[string]bool{"review": true} },
		"no generation":       func() { admission.setGeneration("") },
		"pinning unavailable": func() { admission.setGeneration("gen-3"); admission.refuse = nil; pin.err = ErrPinUnavailable },
	} {
		mutate()
		again, err := service.Trigger(t.Context(), request())
		if err != nil || !again.Duplicate || again.AcceptanceID != first.AcceptanceID {
			t.Fatalf("%s: retry = %+v, %v; want the original acceptance", name, again, err)
		}
	}
	if pin.calls != pinCalls || admission.calls != admitCalls {
		t.Fatal("a replay consulted admission or pinning")
	}
	if string(store.records[first.AcceptanceID].Payload) != before {
		t.Fatal("a replay rewrote the stored envelope (deadline or pins)")
	}
}

func TestKeyReuseForADifferentRequestIsAConflict(t *testing.T) {
	service, _ := newService(&fakeStore{}, &fakeAdmission{generation: "gen-1"})
	if _, err := service.Trigger(t.Context(), request()); err != nil {
		t.Fatal(err)
	}
	other := request()
	other.Workflow = "triage"
	if _, err := service.Trigger(t.Context(), other); status(err) != http.StatusConflict {
		t.Fatalf("workflow reuse = %v, want 409", err)
	}
	intruder := request()
	intruder.Actor = "intruder"
	if _, err := service.Trigger(t.Context(), intruder); status(err) != http.StatusConflict {
		t.Fatalf("actor reuse = %v, want 409", err)
	}
}

// raceStore reports the key unused to the first lookup and then a concurrent
// first acceptance (with its own deadline and pins) to Accept and later reads.
type raceStore struct {
	*fakeStore
	raced bool
}

func (s *raceStore) ByKey(ctx context.Context, key string) (triggerqueue.Record, error) {
	if !s.raced {
		return triggerqueue.Record{}, sql.ErrNoRows
	}
	return s.fakeStore.ByKey(ctx, key)
}

func (s *raceStore) Accept(ctx context.Context, key, actor string, payload []byte, now time.Time) (triggerqueue.Record, bool, error) {
	other, err := (startintent.Envelope{
		Kind: startintent.Kind, Request: startintent.Request{Workflow: "review", Gaggle: "g"},
		Target:   startintent.Target{Gaggle: "g", Workflow: "review", ConfigGeneration: "gen-0", WorkflowDigest: "w0", GooberDigest: "g0"},
		Deadline: now.Add(time.Hour),
	}).Marshal()
	if err != nil {
		return triggerqueue.Record{}, false, err
	}
	if _, _, err := s.fakeStore.Accept(ctx, key, actor, other, now); err != nil {
		return triggerqueue.Record{}, false, err
	}
	s.raced = true
	return s.fakeStore.Accept(ctx, key, actor, payload, now)
}

func TestConcurrentFirstAcceptanceReplaysByRequestNotBytes(t *testing.T) {
	store := &raceStore{fakeStore: &fakeStore{}}
	service, _ := newService(store, &fakeAdmission{generation: "gen-1"})
	response, err := service.Trigger(t.Context(), request())
	if err != nil || !response.Duplicate || response.AcceptanceID != "trigger-key-1" {
		t.Fatalf("racing first acceptance = %+v, %v; want a replay of the winner", response, err)
	}
}

func TestPodContainmentFailsClosed(t *testing.T) {
	store := &fakeStore{}
	admission := &fakeAdmission{generation: "gen-1"}
	service, pin := newService(store, admission)
	pod := request()
	pod.Gaggle, pod.PodScoped, pod.PodRunID = "someone-elses-gaggle", true, "run-in-gaggle-a"

	if _, err := service.Trigger(t.Context(), pod); status(err) != http.StatusForbidden {
		t.Fatalf("no verifier = %v, want 403", err)
	}
	service.Contains = func(_ context.Context, gaggle, runID string) (bool, error) {
		return gaggle == "gaggle-a" && runID == "run-in-gaggle-a", nil
	}
	if _, err := service.Trigger(t.Context(), pod); status(err) != http.StatusForbidden {
		t.Fatalf("cross-gaggle pod = %v, want 403", err)
	}
	service.Contains = func(context.Context, string, string) (bool, error) { return false, errors.New("read model down") }
	if _, err := service.Trigger(t.Context(), pod); status(err) != http.StatusServiceUnavailable {
		t.Fatalf("verifier failure = %v, want 503", err)
	}
	if store.accepts != 0 || admission.calls != 0 || pin.calls != 0 {
		t.Fatal("a refused pod trigger reached admission, pinning or the store")
	}
	service.Contains = func(_ context.Context, gaggle, runID string) (bool, error) {
		return gaggle == "gaggle-a" && runID == "run-in-gaggle-a", nil
	}
	pod.Gaggle = "gaggle-a"
	if _, err := service.Trigger(t.Context(), pod); err != nil {
		t.Fatalf("contained pod trigger = %v", err)
	}
}

func TestTriggerFailsClosed(t *testing.T) {
	for name, tc := range map[string]struct {
		build  func() (*Service, *fakeStore, *fakeAdmission, *recordingPin)
		mutate func(*httpapi.TriggerRequest)
		want   int
		// consulted lists which collaborators the request may touch.
		admits, pins bool
	}{
		"ineligible workflow": {build: func() (*Service, *fakeStore, *fakeAdmission, *recordingPin) {
			a := &fakeAdmission{generation: "g1", refuse: map[string]bool{"review": true}}
			s, p := newService(&fakeStore{}, a)
			return s, s.Store.(*fakeStore), a, p
		}, want: http.StatusUnprocessableEntity, admits: true},
		"no generation": {build: func() (*Service, *fakeStore, *fakeAdmission, *recordingPin) {
			a := &fakeAdmission{}
			s, p := newService(&fakeStore{}, a)
			return s, s.Store.(*fakeStore), a, p
		}, want: http.StatusServiceUnavailable, admits: true},
		"no admission composed": {build: func() (*Service, *fakeStore, *fakeAdmission, *recordingPin) {
			s, p := newService(&fakeStore{}, nil)
			s.Admission = func() Admission { return nil }
			return s, s.Store.(*fakeStore), &fakeAdmission{}, p
		}, want: http.StatusServiceUnavailable},
		"no pin composed": {build: func() (*Service, *fakeStore, *fakeAdmission, *recordingPin) {
			a := &fakeAdmission{generation: "g1"}
			s, p := newService(&fakeStore{}, a)
			s.Pin = nil
			return s, s.Store.(*fakeStore), a, p
		}, want: http.StatusServiceUnavailable},
		"no store": {build: func() (*Service, *fakeStore, *fakeAdmission, *recordingPin) {
			a := &fakeAdmission{generation: "g1"}
			s, p := newService(&fakeStore{}, a)
			s.Store = nil
			return s, &fakeStore{}, a, p
		}, want: http.StatusServiceUnavailable},
		"no key":            {build: freshOK, mutate: func(r *httpapi.TriggerRequest) { r.RequestID = "" }, want: http.StatusBadRequest},
		"no gaggle":         {build: freshOK, mutate: func(r *httpapi.TriggerRequest) { r.Gaggle = "" }, want: http.StatusBadRequest},
		"force with source": {build: freshOK, mutate: func(r *httpapi.TriggerRequest) { r.Force, r.SourceRun = true, "run" }, want: http.StatusBadRequest},
	} {
		service, store, admission, pin := tc.build()
		r := request()
		if tc.mutate != nil {
			tc.mutate(&r)
		}
		if _, err := service.Trigger(t.Context(), r); status(err) != tc.want {
			t.Errorf("%s: %v, want %d", name, err, tc.want)
		}
		if store.accepts != 0 {
			t.Errorf("%s: reached the store", name)
		}
		if !tc.admits && admission.calls != 0 {
			t.Errorf("%s: consulted admission", name)
		}
		if !tc.pins && pin.calls != 0 {
			t.Errorf("%s: consulted pinning", name)
		}
	}
}

func freshOK() (*Service, *fakeStore, *fakeAdmission, *recordingPin) {
	a := &fakeAdmission{generation: "g1"}
	s, p := newService(&fakeStore{}, a)
	return s, s.Store.(*fakeStore), a, p
}

func TestTriggerStatusIsActorAndPodScoped(t *testing.T) {
	store := &fakeStore{}
	service, _ := newService(store, &fakeAdmission{generation: "gen-1"})
	accepted, err := service.Trigger(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	got, err := service.TriggerStatus(t.Context(), httpapi.TriggerStatusRequest{AcceptanceID: accepted.AcceptanceID, Actor: "operator"})
	if err != nil || got.State != string(triggerqueue.Accepted) {
		t.Fatalf("status = %+v, %v", got, err)
	}
	for name, req := range map[string]httpapi.TriggerStatusRequest{
		"other actor": {AcceptanceID: accepted.AcceptanceID, Actor: "intruder"},
		"pod scope":   {AcceptanceID: accepted.AcceptanceID, Actor: "operator", PodScoped: true, PodRunID: "run"},
		"unknown id":  {AcceptanceID: "trigger-nope", Actor: "operator"},
		"oversize id": {AcceptanceID: string(make([]byte, 200)), Actor: "operator"},
	} {
		if _, err := service.TriggerStatus(t.Context(), req); status(err) != http.StatusNotFound {
			t.Errorf("%s: %v, want 404", name, err)
		}
	}
}
