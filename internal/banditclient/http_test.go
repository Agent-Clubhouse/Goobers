package banditclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/bandit"
	"github.com/goobers/goobers/internal/bandit/conformance"
	"github.com/goobers/goobers/internal/banditclient"
	"github.com/goobers/goobers/internal/controlplane/planes"
)

type minter struct{ runs []string }

func (m *minter) Mint(runID string, _ time.Duration) (string, error) {
	m.runs = append(m.runs, runID)
	return "token-for-" + runID, nil
}

func TestObservationHTTPRetriesAmbiguousCommitWithoutDuplicatingSample(t *testing.T) {
	store := &memoryObservationStore{records: make(map[string]bandit.ObservationRecord)}
	service, err := planes.NewBanditService(store)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != apicontract.BanditObservationsPath || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer token-for-run-a" {
			t.Errorf("unexpected request: %s %s %s", r.Method, r.URL.Path, r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusForbidden)
			return
		}
		var input bandit.ObservationRequest
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		output, err := service.Observe(r.Context(), input)
		if errors.Is(err, bandit.ErrObservationConflict) {
			w.WriteHeader(http.StatusConflict)
			return
		}
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if calls.Add(1) == 1 {
			// The outcome committed, but the caller receives an ambiguous budget
			// failure. A replay-safe retry must verify it rather than append it.
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":"request_budget_exceeded","message":"response lost"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(output)
	}))
	t.Cleanup(server.Close)
	m := &minter{}
	client := &banditclient.HTTP{BaseURL: server.URL, Minter: m, Client: server.Client()}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	record := conformance.Record("run-a", "build", 0, 1, 1)
	input := bandit.ObservationRequest{Operation: "record", RunID: "run-a", Stage: "build", Record: &record}
	if _, err := client.Observe(ctx, input); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected committed request plus retry, got %d", calls.Load())
	}
	got, err := client.Observe(ctx, bandit.ObservationRequest{Operation: "read", RunID: "run-a", Stage: "build"})
	if err != nil || len(got.Observations) != 1 || got.Observations[0] != record.Observation {
		t.Fatalf("retry result = %v, %v", got, err)
	}
	record.Observation.Arm = "conflict"
	if _, err := client.Observe(ctx, input); !errors.Is(err, bandit.ErrObservationConflict) {
		t.Fatalf("conflict response = %v", err)
	}
	if len(m.runs) != 3 {
		t.Fatalf("minted %d tokens for three operations", len(m.runs))
	}
}

// This transport test double models the backend's already-qualified immutable
// record contract; production cloud assembly only uses PostgreSQL.
type memoryObservationStore struct {
	mu      sync.Mutex
	records map[string]bandit.ObservationRecord
}

func (s *memoryObservationStore) Record(_ context.Context, record bandit.ObservationRecord) error {
	key, _, err := bandit.EncodeRecord(record)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.records[key]; ok && existing != record {
		return bandit.ErrObservationConflict
	}
	s.records[key] = record
	return nil
}

func (s *memoryObservationStore) Observations(_ context.Context, stage string) ([]bandit.Observation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var observations []bandit.Observation
	for _, record := range s.records {
		if record.Key.Stage == stage {
			observations = append(observations, record.Observation)
		}
	}
	return observations, nil
}

func TestObservationHTTPRequiresBearerBeforeNetwork(t *testing.T) {
	client := &banditclient.HTTP{BaseURL: "http://127.0.0.1:1"}
	_, err := client.Observe(t.Context(), bandit.ObservationRequest{Operation: "read", RunID: "run-a", Stage: "build"})
	if err == nil || err.Error() != "planehttp: bearer token is required" {
		t.Fatalf("missing bearer = %v", err)
	}
}
