package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/readservice"
)

type blockingAggregateReader struct {
	*fakeReader
	entered  chan struct{}
	release  chan struct{}
	canceled chan struct{}
	calls    atomic.Int32
}

func (r *blockingAggregateReader) Instance(ctx context.Context) (readservice.Instance, error) {
	if r.calls.Add(1) == 1 {
		close(r.entered)
	}
	select {
	case <-ctx.Done():
		if r.canceled != nil {
			close(r.canceled)
		}
		return readservice.Instance{}, ctx.Err()
	case <-r.release:
		return readservice.Instance{Name: "shared"}, nil
	}
}

func TestIdenticalAggregateGETsShareOneAdmissionAndRead(t *testing.T) {
	reader := &blockingAggregateReader{
		fakeReader: &fakeReader{},
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	handler, err := NewHandler(reader, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}

	const requests = 6
	responses := make(chan *httptest.ResponseRecorder, requests)
	var started sync.WaitGroup
	started.Add(requests)
	for range requests {
		go func() {
			started.Done()
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, InstancePath, nil))
			responses <- response
		}()
	}
	started.Wait()
	select {
	case <-reader.entered:
	case <-time.After(time.Second):
		t.Fatal("aggregate read did not start")
	}
	time.Sleep(20 * time.Millisecond)
	close(reader.release)

	for range requests {
		select {
		case response := <-responses:
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body)
			}
		case <-time.After(time.Second):
			t.Fatal("aggregate request did not complete")
		}
	}
	if got := reader.calls.Load(); got != 1 {
		t.Fatalf("aggregate reader calls = %d, want 1", got)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, InstancePath, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("sequential status = %d, body = %s", response.Code, response.Body)
	}
	if got := reader.calls.Load(); got != 2 {
		t.Fatalf("aggregate reader calls after sequential request = %d, want 2 (no response cache)", got)
	}
}

func TestCancelledAggregateWaiterDoesNotCancelSharedRead(t *testing.T) {
	reader := &blockingAggregateReader{
		fakeReader: &fakeReader{},
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	handler, err := NewHandler(reader, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	firstRequest := httptest.NewRequest(http.MethodGet, InstancePath, nil).WithContext(ctx)
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, firstRequest)
		firstDone <- response
	}()
	<-reader.entered

	secondDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, InstancePath, nil))
		secondDone <- response
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case response := <-firstDone:
		if response.Code != statusClientClosedRequest {
			t.Fatalf("cancelled waiter status = %d, want %d", response.Code, statusClientClosedRequest)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled waiter did not return")
	}

	close(reader.release)
	select {
	case response := <-secondDone:
		if response.Code != http.StatusOK {
			t.Fatalf("shared waiter status = %d, body = %s", response.Code, response.Body)
		}
	case <-time.After(time.Second):
		t.Fatal("shared waiter did not complete")
	}
	if got := reader.calls.Load(); got != 1 {
		t.Fatalf("aggregate reader calls = %d, want 1", got)
	}
}

func TestAggregateRequestContextStillCarriesServerBudget(t *testing.T) {
	reader := &blockingAggregateReader{
		fakeReader: &fakeReader{},
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	handler, err := NewHandler(reader, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, InstancePath, nil)
	ctx, cancel := context.WithCancel(request.Context())
	cancel()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request.WithContext(ctx))
	if response.Code != statusClientClosedRequest {
		t.Fatalf("status = %d, want %d", response.Code, statusClientClosedRequest)
	}
	if got := reader.calls.Load(); got != 0 {
		t.Fatalf("reader calls = %d, want 0 for an already-cancelled request", got)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("request context error = %v", ctx.Err())
	}
}

func TestSoleAggregateCancellationReachesReader(t *testing.T) {
	reader := &blockingAggregateReader{
		fakeReader: &fakeReader{},
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
		canceled:   make(chan struct{}),
	}
	handler, err := NewHandler(reader, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, InstancePath, nil).WithContext(ctx)
		handler.ServeHTTP(response, request)
		done <- response
	}()
	<-reader.entered
	cancel()

	select {
	case <-reader.canceled:
	case <-time.After(time.Second):
		t.Fatal("reader context was not cancelled after the sole waiter left")
	}
	select {
	case response := <-done:
		if response.Code != statusClientClosedRequest {
			t.Fatalf("status = %d, want %d", response.Code, statusClientClosedRequest)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled aggregate request did not return")
	}
}
