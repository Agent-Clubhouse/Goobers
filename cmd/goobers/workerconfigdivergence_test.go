package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
)

func TestCompareConfigDigests(t *testing.T) {
	tests := []struct {
		name         string
		worker       string
		daemon       string
		fetchErr     error
		wantDiverged bool
		wantMentions []string
	}{
		{
			name:         "agreement is reported as such",
			worker:       "sha256:aaa",
			daemon:       "sha256:aaa",
			wantMentions: []string{"none", "sha256:aaa"},
		},
		{
			// The incident: the worker serves the tree its ConfigMap was
			// seeded from, the daemon has moved on, and every agentic gate is
			// refused until a deploy.
			name:         "divergence names both trees and the remedy",
			worker:       "sha256:5da91786f8630036",
			daemon:       "sha256:2803d658c27b5902",
			wantDiverged: true,
			wantMentions: []string{"sha256:5da91786f8630036", "sha256:2803d658c27b5902", "gate_pin_missing", "DEPLOY", "#4153"},
		},
		{
			// An unreachable daemon is NOT divergence. Reporting one on every
			// restart or network blip would make the alarm unreadable, and an
			// alarm that fires for the wrong reason stops being read.
			name:         "an unreadable daemon digest is not divergence",
			worker:       "sha256:aaa",
			fetchErr:     errors.New("connection refused"),
			wantDiverged: false,
			wantMentions: []string{"NOT CHECKED", "connection refused"},
		},
		{
			// A worker with no snapshot has no position to diverge from.
			name:         "a worker with no tree yet is not divergence",
			daemon:       "sha256:bbb",
			wantDiverged: false,
			wantMentions: []string{"NOT CHECKED", "has not resolved its own config tree"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report := compareConfigDigests(test.worker, test.daemon, test.fetchErr)
			if report.Diverged != test.wantDiverged {
				t.Errorf("Diverged = %v, want %v", report.Diverged, test.wantDiverged)
			}
			message := report.Message()
			for _, want := range test.wantMentions {
				if !strings.Contains(message, want) {
					t.Errorf("message does not mention %q:\n%s", want, message)
				}
			}
		})
	}
}

func TestFetchDaemonConfigDigest(t *testing.T) {
	t.Run("reads the digest and presents the bearer", func(t *testing.T) {
		var gotAuth, gotPath string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
			_ = json.NewEncoder(w).Encode(map[string]string{"digest": "sha256:abc"})
		}))
		t.Cleanup(server.Close)

		digest, err := fetchDaemonConfigDigest(context.Background(), server.Client(), server.URL, "tok")
		if err != nil {
			t.Fatal(err)
		}
		if digest != "sha256:abc" {
			t.Errorf("digest = %q", digest)
		}
		if gotPath != apicontract.ConfigDigestPath {
			t.Errorf("path = %q, want %q", gotPath, apicontract.ConfigDigestPath)
		}
		if gotAuth != "Bearer tok" {
			t.Errorf("Authorization = %q, want the pod bearer", gotAuth)
		}
	})

	t.Run("a refusal is an error, not an empty digest", func(t *testing.T) {
		// An empty digest returned as success would compare unequal to the
		// worker's and be reported as divergence — an alarm raised by an
		// auth failure.
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		t.Cleanup(server.Close)

		if _, err := fetchDaemonConfigDigest(context.Background(), server.Client(), server.URL, ""); err == nil {
			t.Fatal("a 401 was not reported as an error")
		}
	})

	t.Run("an empty digest is an error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]string{"digest": ""})
		}))
		t.Cleanup(server.Close)

		if _, err := fetchDaemonConfigDigest(context.Background(), server.Client(), server.URL, ""); err == nil {
			t.Fatal("an empty digest was accepted as an answer")
		}
	})
}

func TestConfigDigestPublisherTracksTheLiveTree(t *testing.T) {
	// The publisher exists because the API handler is built before the config
	// reloader. A digest that stopped moving would make every worker look
	// converged forever — false assurance, worse than no alarm at all.
	p := newConfigDigestPublisher("sha256:first")
	if got := p.Get(); got != "sha256:first" {
		t.Errorf("Get() = %q", got)
	}
	p.Set("sha256:second")
	if got := p.Get(); got != "sha256:second" {
		t.Errorf("Get() after Set = %q, want the newly applied tree", got)
	}
	if got := (*configDigestPublisher)(nil).Get(); got != "" {
		t.Errorf("nil publisher Get() = %q, want empty", got)
	}
}
