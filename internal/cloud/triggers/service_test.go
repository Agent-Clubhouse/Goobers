package triggers

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

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

func (s *fakeStore) Get(_ context.Context, id, actor string) (triggerqueue.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.records[id]; ok && r.Actor == actor {
		return r, nil
	}
	return triggerqueue.Record{}, sql.ErrNoRows
}

type fakeAdmission struct {
	generation string
	refuse     map[string]bool
}

func (f fakeAdmission) Generation() string { return f.generation }
func (f fakeAdmission) Admit(digest, gaggle, workflow string) error {
	if digest != f.generation || f.refuse[workflow] {
		return errors.New("refused")
	}
	return nil
}

func newService(store Store, admission Admission) *Service {
	return &Service{
		Store:     store,
		Admission: func() Admission { return admission },
		Pin:       func(context.Context, string, string, string) (string, string, error) { return "wf", "goober", nil },
		Now:       func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
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

func TestTriggerAcceptsPinnedEnvelopeAndReplaysByKey(t *testing.T) {
	store := &fakeStore{}
	service := newService(store, fakeAdmission{generation: "gen-1"})
	first, err := service.Trigger(t.Context(), request())
	if err != nil || first.Duplicate || first.State != string(triggerqueue.Accepted) {
		t.Fatalf("first = %+v, %v", first, err)
	}
	again, err := service.Trigger(t.Context(), request())
	if err != nil || !again.Duplicate || again.AcceptanceID != first.AcceptanceID {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	envelope, err := startintent.Parse(store.records[first.AcceptanceID].Payload)
	if err != nil {
		t.Fatal(err)
	}
	want := startintent.Target{Gaggle: "g", Workflow: "review", ConfigGeneration: "gen-1", WorkflowDigest: "wf", GooberDigest: "goober"}
	if envelope.Target != want || envelope.Request.Workflow != "review" {
		t.Fatalf("envelope = %+v, want target %+v", envelope, want)
	}
	changed := request()
	changed.Workflow = "triage"
	if _, err := service.Trigger(t.Context(), changed); status(err) != http.StatusConflict {
		t.Fatalf("key reuse = %v, want 409", err)
	}
}

func TestTriggerFailsClosed(t *testing.T) {
	store := &fakeStore{}
	for name, tc := range map[string]struct {
		service *Service
		mutate  func(*httpapi.TriggerRequest)
		want    int
	}{
		"ineligible workflow": {newService(store, fakeAdmission{generation: "g1", refuse: map[string]bool{"review": true}}), nil, http.StatusUnprocessableEntity},
		"no generation":       {newService(store, fakeAdmission{}), nil, http.StatusServiceUnavailable},
		"no admission":        {&Service{Store: store, Pin: newService(store, nil).Pin}, nil, http.StatusServiceUnavailable},
		"no pin":              {&Service{Store: store, Admission: func() Admission { return fakeAdmission{generation: "g1"} }}, nil, http.StatusServiceUnavailable},
		"no key":              {newService(store, fakeAdmission{generation: "g1"}), func(r *httpapi.TriggerRequest) { r.RequestID = "" }, http.StatusBadRequest},
		"no gaggle":           {newService(store, fakeAdmission{generation: "g1"}), func(r *httpapi.TriggerRequest) { r.Gaggle = "" }, http.StatusBadRequest},
		"force with source":   {newService(store, fakeAdmission{generation: "g1"}), func(r *httpapi.TriggerRequest) { r.Force, r.SourceRun = true, "run" }, http.StatusBadRequest},
	} {
		r := request()
		if tc.mutate != nil {
			tc.mutate(&r)
		}
		if _, err := tc.service.Trigger(t.Context(), r); status(err) != tc.want {
			t.Errorf("%s: %v, want %d", name, err, tc.want)
		}
	}
	if store.accepts != 0 {
		t.Fatalf("refused triggers reached the store %d times", store.accepts)
	}
}

func TestTriggerStatusIsActorAndPodScoped(t *testing.T) {
	store := &fakeStore{}
	service := newService(store, fakeAdmission{generation: "gen-1"})
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
