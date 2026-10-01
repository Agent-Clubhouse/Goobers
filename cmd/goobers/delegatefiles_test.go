package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type delegateFileTestRequest struct {
	Value     string    `json:"value,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

type delegateFileTestResponse struct {
	Value string `json:"value,omitempty"`
	Error string `json:"error,omitempty"`
}

func delegateFileTestProtocol() delegateFileProtocol {
	return delegateFileProtocol{
		pendingDir:     "pending-tests",
		requestSuffix:  ".request.json",
		responseSuffix: ".response.json",
		errorPrefix:    "test delegate",
		staleAfter:     time.Minute,
	}
}

func TestWriteDelegateRequestPublishesHiddenTempAtomically(t *testing.T) {
	schedulerDir := t.TempDir()
	cfg := delegateFileTestProtocol()
	createdAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	requestID, err := writeDelegateRequest(
		schedulerDir,
		cfg,
		delegateFileTestRequest{Value: "ready"},
		func(req *delegateFileTestRequest) { req.CreatedAt = createdAt },
	)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(schedulerDir, cfg.pendingDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != requestID+cfg.requestSuffix {
		t.Fatalf("published entries = %v, want only %s", entryNames(entries), requestID+cfg.requestSuffix)
	}
	if strings.HasPrefix(entries[0].Name(), ".pending-") {
		t.Fatalf("temporary request remained published: %s", entries[0].Name())
	}
	data, err := os.ReadFile(filepath.Join(schedulerDir, cfg.pendingDir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var req delegateFileTestRequest
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatal(err)
	}
	if req.Value != "ready" || !req.CreatedAt.Equal(createdAt) {
		t.Fatalf("request = %+v, want mutated request", req)
	}
}

func TestPollDelegateResponseToleratesMalformedJSONAndConsumesResponse(t *testing.T) {
	schedulerDir := t.TempDir()
	cfg := delegateFileTestProtocol()
	reqDir := filepath.Join(schedulerDir, cfg.pendingDir)
	if err := os.MkdirAll(reqDir, 0o755); err != nil {
		t.Fatal(err)
	}
	respPath := filepath.Join(reqDir, "request-1"+cfg.responseSuffix)
	if err := os.WriteFile(respPath, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeErr := make(chan error, 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		writeErr <- os.WriteFile(respPath, []byte(`{"value":"done"}`), 0o644)
	}()

	resp, err := pollDelegateResponse[delegateFileTestResponse](
		context.Background(),
		schedulerDir,
		"request-1",
		cfg,
		time.Second,
		func(string) string { return "timed out" },
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}
	if resp.Value != "done" {
		t.Fatalf("response = %+v, want done", resp)
	}
	if _, err := os.Stat(respPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("response was not consumed: %v", err)
	}
}

func TestSweepDelegateRequestsCharacterizesValidationAndOrdering(t *testing.T) {
	schedulerDir := t.TempDir()
	cfg := delegateFileTestProtocol()
	reqDir := filepath.Join(schedulerDir, cfg.pendingDir)
	if err := os.MkdirAll(reqDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	requests := map[string][]byte{
		"malformed": []byte("{"),
		"missing":   []byte(`{"value":"missing"}`),
		"stale": marshalDelegateTestJSON(t, delegateFileTestRequest{
			Value:     "stale",
			CreatedAt: now.Add(-2 * cfg.staleAfter),
		}),
		"valid": marshalDelegateTestJSON(t, delegateFileTestRequest{
			Value:     "valid",
			CreatedAt: now,
		}),
	}
	for requestID, data := range requests {
		if err := os.WriteFile(filepath.Join(reqDir, requestID+cfg.requestSuffix), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	staleResponsePath := filepath.Join(reqDir, "orphan"+cfg.responseSuffix)
	if err := os.WriteFile(staleResponsePath, []byte(`{"value":"old"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(staleResponsePath, now.Add(-2*cfg.staleAfter), now.Add(-2*cfg.staleAfter)); err != nil {
		t.Fatal(err)
	}

	dispatches := 0
	err := sweepDelegateRequests(
		schedulerDir,
		cfg,
		func() time.Time { return now },
		func(requestID string, req delegateFileTestRequest, decodeErr error) (delegateFileTestResponse, bool) {
			switch {
			case decodeErr != nil:
				return delegateFileTestResponse{Error: "malformed"}, false
			case req.CreatedAt.IsZero():
				return delegateFileTestResponse{Error: "missing creation time"}, false
			case now.Sub(req.CreatedAt) > cfg.staleAfter:
				return delegateFileTestResponse{Error: "stale request"}, false
			default:
				return delegateFileTestResponse{}, true
			}
		},
		func(req delegateFileTestRequest) delegateFileTestResponse {
			dispatches++
			requestPath := filepath.Join(reqDir, "valid"+cfg.requestSuffix)
			if _, err := os.Stat(requestPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("request existed during dispatch: %v", err)
			}
			return delegateFileTestResponse{Value: req.Value}
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if dispatches != 1 {
		t.Fatalf("dispatches = %d, want 1", dispatches)
	}
	if _, err := os.Stat(staleResponsePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale response was not removed: %v", err)
	}

	want := map[string]delegateFileTestResponse{
		"malformed": {Error: "malformed"},
		"missing":   {Error: "missing creation time"},
		"stale":     {Error: "stale request"},
		"valid":     {Value: "valid"},
	}
	for requestID, expected := range want {
		data, err := os.ReadFile(filepath.Join(reqDir, requestID+cfg.responseSuffix))
		if err != nil {
			t.Fatal(err)
		}
		var got delegateFileTestResponse
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		if got != expected {
			t.Fatalf("%s response = %+v, want %+v", requestID, got, expected)
		}
	}
}

func TestPollDelegateResponseTimeoutAndContextCancellation(t *testing.T) {
	cfg := delegateFileTestProtocol()
	schedulerDir := t.TempDir()

	_, err := pollDelegateResponse[delegateFileTestResponse](
		context.Background(),
		schedulerDir,
		"timeout",
		cfg,
		time.Millisecond,
		func(path string) string { return "timeout waiting at " + path },
	)
	if err == nil || !strings.Contains(err.Error(), "timeout waiting at ") ||
		!strings.HasSuffix(err.Error(), filepath.Join(cfg.pendingDir, "timeout"+cfg.requestSuffix)) {
		t.Fatalf("timeout error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = pollDelegateResponse[delegateFileTestResponse](
		ctx,
		schedulerDir,
		"cancelled",
		cfg,
		time.Second,
		func(string) string { return "timed out" },
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v, want context canceled", err)
	}
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func marshalDelegateTestJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
