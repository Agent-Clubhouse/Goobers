package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestADOStateMetadataProviderCacheExpires(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"value":[{"name":"Active","category":"InProgress"}]}`))
	}))
	defer server.Close()
	now := time.Date(2026, 10, 20, 12, 1, 0, 0, time.UTC)
	p := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL; p.now = func() time.Time { return now } })
	read := func() {
		t.Helper()
		if _, err := p.adoWorkItemStateCategories(context.Background(), RepositoryRef{Project: "project"}, "Task"); err != nil {
			t.Fatal(err)
		}
	}
	read()
	read()
	if calls.Load() != 1 {
		t.Fatalf("same-window reads=%d", calls.Load())
	}
	now = now.Truncate(ADOStateMetadataTTL).Add(ADOStateMetadataTTL)
	read()
	if calls.Load() != 2 {
		t.Fatalf("expired reads=%d", calls.Load())
	}
	now = now.Add(-time.Minute)
	read()
	if calls.Load() != 3 {
		t.Fatalf("backward clock reused future entry: reads=%d", calls.Load())
	}
}

func TestADOStateMetadataProviderDoesNotCacheErrors(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "unavailable", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"value":[{"name":"Active","category":"InProgress"}]}`))
	}))
	defer server.Close()
	p := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	repo := RepositoryRef{Project: "project"}
	if _, err := p.adoWorkItemStateCategories(t.Context(), repo, "Task"); err == nil {
		t.Fatal("expected first failure")
	}
	if _, err := p.adoWorkItemStateCategories(t.Context(), repo, "Task"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls=%d", calls.Load())
	}
}
