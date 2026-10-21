package temporalcli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type fakeClock struct {
	now    time.Time
	sleeps []time.Duration
}

func (c *fakeClock) sleep(_ context.Context, delay time.Duration) error {
	c.sleeps = append(c.sleeps, delay)
	c.now = c.now.Add(delay)
	return nil
}

func TestGetRetryClassification(t *testing.T) {
	tests := []struct {
		name         string
		firstStatus  int
		retryAfter   string
		wantRequests int
		wantSleeps   []time.Duration
		wantErr      bool
	}{
		{
			name:         "429 retries and honors Retry-After",
			firstStatus:  http.StatusTooManyRequests,
			retryAfter:   "3",
			wantRequests: 2,
			wantSleeps:   []time.Duration{3 * time.Second},
		},
		{
			name:         "404 is permanent",
			firstStatus:  http.StatusNotFound,
			wantRequests: 1,
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests++
				if requests == 1 {
					if tt.retryAfter != "" {
						w.Header().Set("Retry-After", tt.retryAfter)
					}
					http.Error(w, http.StatusText(tt.firstStatus), tt.firstStatus)
					return
				}
				_, _ = io.WriteString(w, "ok")
			}))
			defer server.Close()

			clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
			d := newDownloader()
			d.client = server.Client()
			d.now = func() time.Time { return clock.now }
			d.sleep = clock.sleep
			d.jitter = func(time.Duration) time.Duration { return 0 }

			err := d.get(context.Background(), server.URL, func(body io.Reader) error {
				_, err := io.ReadAll(body)
				return err
			})
			if (err != nil) != tt.wantErr {
				t.Fatalf("get() error = %v, wantErr %t", err, tt.wantErr)
			}
			if requests != tt.wantRequests {
				t.Errorf("requests = %d, want %d", requests, tt.wantRequests)
			}
			if len(clock.sleeps) != len(tt.wantSleeps) {
				t.Fatalf("sleeps = %v, want %v", clock.sleeps, tt.wantSleeps)
			}
			for i := range tt.wantSleeps {
				if clock.sleeps[i] != tt.wantSleeps[i] {
					t.Errorf("sleep[%d] = %s, want %s", i, clock.sleeps[i], tt.wantSleeps[i])
				}
			}
		})
	}
}
