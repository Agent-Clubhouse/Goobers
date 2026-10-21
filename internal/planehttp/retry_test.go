package planehttp

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/retryutil"
)

func fastRetry() RetryConfig {
	return RetryConfig{
		MaxElapsed: 5 * time.Second,
		Backoff:    retryutil.Policy{Base: time.Millisecond, Max: 4 * time.Millisecond},
	}
}

func newTestClient(t *testing.T, base string, httpClient *http.Client, retry RetryConfig) *Client {
	t.Helper()
	client, err := New(Config{BaseURL: base, Token: "t", Client: httpClient, Retry: retry})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func envelope(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"error":{"code":"`+code+`","message":"m"}}`)
}

func TestRetriesOverloadStatusesThenSucceeds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		code   string
		method string
		safe   bool
	}{
		{"saturated GET", 503, "class_saturated", http.MethodGet, false},
		{"saturated unsafe POST", 503, "class_saturated", http.MethodPost, false},
		{"429 unsafe POST", 429, "class_saturated", http.MethodPost, false},
		{"recovering unsafe PUT", 503, "recovering", http.MethodPut, false},
		{"budget GET", 503, "request_budget_exceeded", http.MethodGet, false},
		{"budget safe POST", 503, "request_budget_exceeded", http.MethodPost, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				if string(raw) != "payload" && r.Method != http.MethodGet {
					t.Errorf("body not replayed: %q", raw)
				}
				if calls.Add(1) <= 3 {
					envelope(w, tc.status, tc.code)
					return
				}
				_, _ = io.WriteString(w, "ok")
			}))
			defer server.Close()
			client := newTestClient(t, server.URL, nil, fastRetry())
			var body []byte
			if tc.method != http.MethodGet {
				body = []byte("payload")
			}
			response, err := client.DoRetrying(context.Background(), tc.method, "/x", body, nil, tc.safe)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			raw, _ := io.ReadAll(response.Body)
			if response.StatusCode != 200 || string(raw) != "ok" || calls.Load() != 4 {
				t.Fatalf("status=%d body=%q calls=%d", response.StatusCode, raw, calls.Load())
			}
		})
	}
}

func TestOtherCoded429IsNotRetried(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		envelope(w, 429, "quota_exhausted")
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, nil, fastRetry())
	response, err := client.DoRetrying(context.Background(), http.MethodGet, "/x", nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if calls.Load() != 1 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestDoesNotRetryPermanentErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{
		{400, "invalid_request"}, {401, "unauthenticated"}, {403, "forbidden"}, {404, "not_found"},
		{409, "conflict"}, {412, "precondition"}, {500, "read_error"},
	} {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			envelope(w, tc.status, tc.code)
		}))
		client := newTestClient(t, server.URL, nil, fastRetry())
		response, err := client.DoRetrying(context.Background(), http.MethodGet, "/x", nil, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		server.Close()
		if response.StatusCode != tc.status || calls.Load() != 1 || !strings.Contains(string(raw), tc.code) {
			t.Fatalf("%d %s: status=%d calls=%d body=%s", tc.status, tc.code, response.StatusCode, calls.Load(), raw)
		}
	}
}

func TestAmbiguousBudgetExpiryNotReplayedForUnsafeMutation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		envelope(w, 503, "request_budget_exceeded")
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, nil, fastRetry())
	response, err := client.DoRetrying(context.Background(), http.MethodPost, "/x", []byte("p"), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 503 || calls.Load() != 1 || !strings.Contains(string(raw), "request_budget_exceeded") {
		t.Fatalf("status=%d calls=%d body=%s", response.StatusCode, calls.Load(), raw)
	}
}

func TestExhaustedStatusReturnsLastResponseWithBody(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		envelope(w, 503, "class_saturated")
	}))
	defer server.Close()
	clock := newRetryFakeClock()
	client := newTestClient(t, server.URL, nil, clock.config(5*time.Second, time.Second))
	response, err := client.DoRetrying(context.Background(), http.MethodGet, "/x", nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 503 || calls.Load() < 2 || !strings.Contains(string(raw), "class_saturated") {
		t.Fatalf("status=%d calls=%d body=%s", response.StatusCode, calls.Load(), raw)
	}
}

func TestRetryRespectsContextDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		envelope(w, 503, "class_saturated")
	}))
	defer server.Close()
	cfg := RetryConfig{MaxElapsed: time.Minute, Backoff: retryutil.Policy{Base: 200 * time.Millisecond, Max: 200 * time.Millisecond}}
	client := newTestClient(t, server.URL, nil, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	response, err := client.DoRetrying(ctx, http.MethodGet, "/x", nil, nil, true)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("retry outlived the context deadline: %v", elapsed)
	}
	if err == nil {
		_ = response.Body.Close()
	}
}

func TestRetriesRealHTTP2StreamReset(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= 2 {
			panic(http.ErrAbortHandler) // HTTP/2: RST_STREAM(INTERNAL_ERROR), nothing logged
		}
		_, _ = io.WriteString(w, "ok")
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	client := newTestClient(t, server.URL, server.Client(), fastRetry())

	// Unsafe mutation: the reset is ambiguous, so it is not replayed.
	_, err := client.DoRetrying(context.Background(), http.MethodPost, "/x", []byte("p"), nil, false)
	if err == nil || !strings.Contains(err.Error(), "INTERNAL_ERROR") {
		t.Fatalf("unsafe mutation err = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("unsafe mutation replayed: calls=%d", calls.Load())
	}
	// Read: replayed through the second reset to success.
	response, err := client.DoRetrying(context.Background(), http.MethodGet, "/x", nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if string(raw) != "ok" || calls.Load() != 3 {
		t.Fatalf("body=%q calls=%d", raw, calls.Load())
	}
}

type scriptedTransport struct {
	errs  []error
	calls atomic.Int32
}

func (s *scriptedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	n := int(s.calls.Add(1)) - 1
	if n < len(s.errs) {
		return nil, s.errs[n]
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok")), Header: http.Header{}, Request: r}, nil
}

func TestTransportErrorClassification(t *testing.T) {
	dial := &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	timeout := &net.DNSError{IsTimeout: true}
	for _, tc := range []struct {
		name       string
		err        error
		wantRead   int32 // calls for a GET
		wantUnsafe int32 // calls for an unsafe POST
	}{
		{"stream reset internal", errors.New("stream error: stream ID 5; INTERNAL_ERROR; received from peer"), 2, 1},
		{"stream refused", errors.New("stream error: stream ID 5; REFUSED_STREAM; received from peer"), 2, 2},
		{"stream cancel", errors.New("stream error: stream ID 5; CANCEL"), 1, 1},
		{"eof", io.EOF, 2, 1},
		{"unexpected eof", io.ErrUnexpectedEOF, 2, 1},
		{"conn reset", errors.New("read tcp: connection reset by peer"), 2, 1},
		{"dial refused", dial, 2, 2},
		{"timeout", timeout, 2, 1},
		{"canceled", context.Canceled, 1, 1},
		{"other", errors.New("x509: certificate signed by unknown authority"), 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []struct {
				method string
				want   int32
			}{{http.MethodGet, tc.wantRead}, {http.MethodPost, tc.wantUnsafe}} {
				transport := &scriptedTransport{errs: []error{tc.err}}
				client := newTestClient(t, "http://daemon", &http.Client{Transport: transport}, fastRetry())
				response, err := client.DoRetrying(context.Background(), mode.method, "/x", []byte("p"), nil, false)
				if err == nil {
					_ = response.Body.Close()
				}
				if got := transport.calls.Load(); got != mode.want {
					t.Fatalf("%s: calls = %d, want %d (err=%v)", mode.method, got, mode.want, err)
				}
			}
		})
	}
}

func TestReplaySafeMutationRetriesAmbiguousTransportError(t *testing.T) {
	transport := &scriptedTransport{errs: []error{io.EOF, errors.New("stream error: stream ID 1; INTERNAL_ERROR; received from peer")}}
	client := newTestClient(t, "http://daemon", &http.Client{Transport: transport}, fastRetry())
	response, err := client.DoJSONRetrying(context.Background(), http.MethodPost, "/x", map[string]string{"a": "b"}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if transport.calls.Load() != 3 {
		t.Fatalf("calls = %d", transport.calls.Load())
	}
}

func TestExhaustedTransportErrorNamesAttempts(t *testing.T) {
	errs := make([]error, 100)
	for i := range errs {
		errs[i] = io.EOF
	}
	transport := &scriptedTransport{errs: errs}
	clock := newRetryFakeClock()
	client := newTestClient(t, "http://daemon", &http.Client{Transport: transport}, clock.config(5*time.Second, time.Second))
	_, err := client.DoRetrying(context.Background(), http.MethodGet, "/x", nil, nil, true)
	var requestErr *RequestError
	if !errors.As(err, &requestErr) || !errors.Is(err, io.EOF) || !strings.Contains(err.Error(), "attempt") {
		t.Fatalf("err = %v", err)
	}
}

func TestStatusAndSafetyClassification(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		code       string
		replaySafe bool
		wantCalls  int32
	}{
		{"bare 502 unsafe", 502, "", false, 1},
		{"bare 502 safe", 502, "", true, 2},
		{"bare 503 unsafe", 503, "", false, 2},
		{"bare 503 safe", 503, "", true, 2},
		{"coded 503 unsafe", 503, "claims_unavailable", false, 1},
		{"coded 503 safe", 503, "claims_unavailable", true, 2},
		{"bare 504 unsafe", 504, "", false, 1},
		{"bare 504 safe", 504, "", true, 2},
		{"400 unsafe", 400, "invalid_request", false, 1},
		{"400 safe", 400, "invalid_request", true, 1},
		{"401 safe", 401, "unauthenticated", true, 1},
		{"404 safe", 404, "not_found", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					if tc.code == "" {
						w.WriteHeader(tc.status)
					} else {
						envelope(w, tc.status, tc.code)
					}
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			clock := newRetryFakeClock()
			client := newTestClient(t, server.URL, nil, clock.config(time.Minute, time.Second))
			response, err := client.DoRetrying(context.Background(), http.MethodPost, "/x", nil, nil, tc.replaySafe)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if got := calls.Load(); got != tc.wantCalls {
				t.Fatalf("calls = %d, want %d", got, tc.wantCalls)
			}
		})
	}
}

func TestRetryBudgetIsTimeBoundedNotAttemptBounded(t *testing.T) {
	if DefaultRetryMaxElapsed != 240*time.Second {
		t.Fatalf("default retry window = %v, want 240s", DefaultRetryMaxElapsed)
	}

	t.Run("more than eight attempts may succeed", func(t *testing.T) {
		errs := make([]error, 12)
		for i := range errs {
			errs[i] = io.EOF
		}
		transport := &scriptedTransport{errs: errs}
		clock := newRetryFakeClock()
		client := newTestClient(t, "http://daemon", &http.Client{Transport: transport}, clock.config(time.Minute, time.Millisecond))
		response, err := client.DoRetrying(context.Background(), http.MethodGet, "/x", nil, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if got := transport.calls.Load(); got != 13 {
			t.Fatalf("calls = %d, want 13", got)
		}
	})

	t.Run("elapsed window stops retries", func(t *testing.T) {
		errs := make([]error, 100)
		for i := range errs {
			errs[i] = io.EOF
		}
		transport := &scriptedTransport{errs: errs}
		clock := newRetryFakeClock()
		const window = 5 * time.Second
		client := newTestClient(t, "http://daemon", &http.Client{Transport: transport}, clock.config(window, time.Second))
		_, err := client.DoRetrying(context.Background(), http.MethodGet, "/x", nil, nil, false)
		if err == nil {
			t.Fatal("expected retry exhaustion")
		}
		if clock.elapsed >= window {
			t.Fatalf("fake clock advanced beyond window: %v", clock.elapsed)
		}
		if got := transport.calls.Load(); got < 2 || got >= int32(len(errs)) {
			t.Fatalf("calls = %d, want time-bounded retry count", got)
		}
	})
}

type retryFakeClock struct {
	now     time.Time
	elapsed time.Duration
}

func newRetryFakeClock() *retryFakeClock {
	return &retryFakeClock{now: time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)}
}

func (c *retryFakeClock) config(window, delay time.Duration) RetryConfig {
	return RetryConfig{
		MaxElapsed: window,
		Backoff:    retryutil.Policy{Base: delay, Max: delay},
		now: func() time.Time {
			return c.now.Add(c.elapsed)
		},
		wait: func(_ context.Context, d time.Duration) bool {
			c.elapsed += d
			return true
		},
	}
}

type failingBody struct{ sent bool }

func (b *failingBody) Read(p []byte) (int, error) {
	if b.sent {
		return 0, errors.New("connection reset mid-body")
	}
	b.sent = true
	return copy(p, `{"error":{"code":"x`), nil
}

func TestOverloadBodyReadFailureReturnsErrorNotTruncatedBody(t *testing.T) {
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}, Body: io.NopCloser(&failingBody{})}, nil
	})
	client := newTestClient(t, "http://plane.test", &http.Client{Transport: rt}, fastRetry())
	response, err := client.DoRetrying(context.Background(), http.MethodGet, "/x", nil, nil, false)
	if err == nil || response != nil {
		t.Fatalf("want error and no response, got %v, %v", response, err)
	}
	var requestErr *RequestError
	if !errors.As(err, &requestErr) || requestErr.Op != "read" {
		t.Fatalf("err = %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
