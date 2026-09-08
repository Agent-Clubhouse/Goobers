package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/prqueue"
	"github.com/goobers/goobers/internal/readservice"
)

type queueExplainFake struct {
	view             readservice.QueueEligibilityView
	err              error
	gaggle, workflow string
}

func (f *queueExplainFake) QueueEligibility(_ context.Context, gaggle, workflow string) (readservice.QueueEligibilityView, error) {
	f.gaggle, f.workflow = gaggle, workflow
	return f.view, f.err
}

func queueExplainFixture() readservice.QueueEligibilityView {
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	report := &prqueue.Report{Version: 1, RepositoryKey: "github|||org|repo|", Gaggle: "core", Workflow: "review", RunID: "queue-run", ObservedAt: now, Items: []prqueue.Item{}}
	report.Add(42, prqueue.Escalated)
	report.Items[0].Claim = prqueue.ObserveClaim(true, "", "queue-run", time.Time{}, now, true)
	return readservice.QueueEligibilityView{Gaggle: "core", Workflow: "review", AsOf: now, Status: "observed", SourceRunID: "queue-run", Report: report}
}

func TestQueueExplainScopedHistoricalEvidence(t *testing.T) {
	fake := &queueExplainFake{view: queueExplainFixture()}
	var out, stderr bytes.Buffer
	if code := explainQueueEvidence(context.Background(), fake, "core", "review", 42, false, &out, &stderr); code != 0 {
		t.Fatalf("code=%d: %s", code, &stderr)
	}
	for _, want := range []string{"historical selection evidence", "not permission to claim or merge", "2026-09-08T00:00:00Z", "complete snapshot=false", "PR #42", "provider-label=true", "provider-label-without-live-local-lease", "next:"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q: %s", want, &out)
		}
	}
	if fake.gaggle != "core" || fake.workflow != "review" {
		t.Fatalf("scope lost: %+v", fake)
	}
}

func TestQueueExplainMissingIsUnknownAndJSONRetainsAccounting(t *testing.T) {
	fake := &queueExplainFake{view: queueExplainFixture()}
	var out, stderr bytes.Buffer
	if code := explainQueueEvidence(context.Background(), fake, "core", "review", 99, false, &out, &stderr); code != 1 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(out.String(), "eligibility is unknown") {
		t.Fatal(out.String())
	}
	out.Reset()
	if code := explainQueueEvidence(context.Background(), fake, "core", "review", 99, true, &out, &stderr); code != 1 {
		t.Fatalf("code=%d: %s", code, &stderr)
	}
	var result struct {
		RequestedPR int
		Found       bool
		Evidence    readservice.QueueEligibilityView
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Found || result.RequestedPR != 99 || result.Evidence.Report.MatchingItems != 1 || len(result.Evidence.Report.Items) != 1 {
		t.Fatalf("falsified report: %+v", result)
	}
}

func TestQueueExplainUnavailableAndErrors(t *testing.T) {
	for _, status := range []string{"unavailable", "not-observed"} {
		fake := &queueExplainFake{view: readservice.QueueEligibilityView{Status: status, Problem: "No verified evidence."}}
		if code := explainQueueEvidence(context.Background(), fake, "core", "review", 42, false, io.Discard, io.Discard); code != 1 {
			t.Fatalf("%s code=%d", status, code)
		}
	}
	fake := &queueExplainFake{err: errors.New("read failed")}
	if code := explainQueueEvidence(context.Background(), fake, "core", "review", 42, false, io.Discard, io.Discard); code != 2 {
		t.Fatalf("read error code=%d", code)
	}
}

func TestQueueEvidenceEscapesTerminalControlsAndShowsExpiry(t *testing.T) {
	view := queueExplainFixture()
	expiry := view.Report.ObservedAt.Add(time.Hour)
	view.Report.Items[0].Claim = prqueue.ObserveClaim(true, "other-run", view.Report.RunID, expiry, view.Report.ObservedAt, false)
	view.Report.Items[0].Reason = "untrusted\x1b[2Jreason"
	view.Report.Items[0].NextStep = "line\nspoofed status"
	var out bytes.Buffer
	if err := writeQueueEvidence(&out, view, 42); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\x1b") || strings.Contains(out.String(), "\nspoofed status") {
		t.Fatalf("terminal injection: %q", out.String())
	}
	if !strings.Contains(out.String(), "lease expiry: 2026-09-08T01:00:00Z") {
		t.Fatal(out.String())
	}
	if err := writeQueueEvidence(queueFailWriter{}, view, 42); err == nil {
		t.Fatal("output failure suppressed")
	}
}

type queueFailWriter struct{}

func (queueFailWriter) Write([]byte) (int, error) { return 0, errors.New("output closed") }
