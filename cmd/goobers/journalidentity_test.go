package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/diagnostics/executiondeadline"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/mutationsidecar"
	"github.com/goobers/goobers/providers"
)

type identityEventRecorder func(journal.Event) error

func (f identityEventRecorder) Append(event journal.Event) error { return f(event) }

func TestNonsecretRandomIdentitiesSurviveProductionScrubber(t *testing.T) {
	const samples = 2048
	registry, scrubber := journal.DefaultScrubber()
	registry.Register([]byte("fixture-provider-credential"))
	// This synthetic base32 ID demonstrates why uppercase random identities
	// cannot safely cross the credential scrubber unchanged.
	if raw := []byte(`{"id":"AKIAABCDEFGHIJKLMNOPQRSTUV"}`); bytes.Equal(raw, scrubber.Scrub(raw)) {
		t.Fatal("credential-shaped negative control was not scrubbed")
	}
	check := func(t *testing.T, id string, payload any, seen map[string]bool) {
		t.Helper()
		// At least 26 base32 symbols retain rand.Text's >=128-bit entropy.
		// The lowercase alphabet cannot form an AWS key or any punctuation-
		// delimited credential pattern, unlike uppercase base32.
		if len(id) < 26 || strings.Trim(id, "abcdefghijklmnopqrstuvwxyz234567") != "" {
			t.Fatalf("identifier lacks the scrub-stable base32 alphabet: %q", id)
		}
		if seen[id] {
			t.Fatal("distinct generated identities collided")
		}
		seen[id] = true
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(raw, scrubber.Scrub(raw)) {
			t.Fatal("production scrubber changed an identity-bearing payload")
		}
	}
	t.Run("receipt", func(t *testing.T) {
		workspace := t.TempDir()
		for range samples {
			if err := appendMutationFactAt(workspace, mutationFact{Provider: "github", Kind: "issue", ID: "1", Operation: "claim"}); err != nil {
				t.Fatal(err)
			}
		}
		facts := readMutationFacts(t, workspace)
		if len(facts) != samples {
			t.Fatalf("receipt count = %d, want %d", len(facts), samples)
		}
		seen := make(map[string]bool)
		for _, fact := range facts {
			check(t, fact.ReceiptID, fact, seen)
		}
	})
	t.Run("fleet-boot", func(t *testing.T) {
		root, now := t.TempDir(), time.Now().UTC()
		seen := make(map[string]bool)
		for range samples {
			sample := newFleetHealthSampler(root, nil, &instance.DiagnosticsConfig{}, &fleetTestReader{now: now}, nil)
			record := sample(t.Context(), now)[0]
			check(t, record.Attributes["bootId"].(string), record, seen)
		}
	})
	t.Run("execution", func(t *testing.T) {
		seen := make(map[string]bool)
		var active string
		recorder := identityEventRecorder(func(event journal.Event) error {
			id := event.Runner["executionId"].(string)
			if event.Runner["executionState"] == "active" {
				check(t, id, event, seen)
				active = id
			} else if id != active {
				t.Fatal("execution finish lost its active identity")
			}
			return nil
		})
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		ctx = executiondeadline.WithRecorder(ctx, recorder, "work", 1)
		for range samples {
			_, done := invoke.BeginExecution(ctx)
			done()
		}
		if len(seen) != samples {
			t.Fatalf("execution count = %d, want %d", len(seen), samples)
		}
	})
}

func TestMutationReceiptIdentityAllowsCleanupWithScrubbedLiveJournal(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "generated"
		if legacy {
			name = "legacy-uppercase"
		}
		t.Run(name, func(t *testing.T) {
			workspace, root := t.TempDir(), t.TempDir()
			fact := mutationFact{Provider: "github", Kind: "issue", ID: "1", Operation: "claim", RunID: "owner"}
			if err := appendMutationFactAt(workspace, fact); err != nil {
				t.Fatal(err)
			}
			fact = readMutationFacts(t, workspace)[0]
			if legacy {
				fact.ReceiptID = "LEGACYABCDEFGHIJKLMNOPQRST"
				data, err := json.Marshal(fact)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(workspace, mutationsSidecarFile), append(data, '\n'), 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, scrubber := journal.DefaultScrubber()
			writer, err := journal.Create(root, journal.RunIdentity{RunID: "owner", Workflow: "implementation", Gaggle: "test"}, nil, journal.WithScrubber(scrubber))
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			if err := writer.Append(journal.WithMutationOutcome(journal.Event{
				Type: journal.EventRefTouched, ExternalRef: &journal.ExternalRef{Provider: fact.Provider, Kind: fact.Kind, ID: fact.ID},
				Runner: providers.MutationReceiptRunnerFields(fact.ReceiptID, fact.Operation, nil, nil, nil),
			}, fact.RunID, "", "", "")); err != nil {
				t.Fatal(err)
			}
			// Keeping the writer open ensures cleanup must recognize the durable
			// projection; attempting recovery instead would return ErrRecoveryBusy.
			if err := mutationsidecar.RecoverBeforeCleanup(t.Context(), workspace, "owner-query-backlog", "owner", writer.Dir()); err != nil {
				t.Fatalf("durably projected receipt blocks cleanup: %v", err)
			}
		})
	}
}
