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

	"github.com/goobers/goobers/internal/readservice"
)

type fakeCostReader struct {
	request readservice.TelemetryCostRequest
	result  readservice.TelemetryCostResult
	err     error
}

func (f *fakeCostReader) TelemetryCosts(_ context.Context, request readservice.TelemetryCostRequest) (readservice.TelemetryCostResult, error) {
	f.request = request
	return f.result, f.err
}

type costFailWriter struct{}

func (costFailWriter) Write([]byte) (int, error) { return 0, errors.New("closed pipe") }

func TestCostCommandQueriesPRAndWritesDeterministicJSON(t *testing.T) {
	now := time.Date(2026, 9, 7, 1, 2, 3, 0, time.UTC)
	reader := &fakeCostReader{result: readservice.TelemetryCostResult{
		Provider: "github", Scope: "pr", ExternalID: "4398",
		Since: now.Add(-24 * time.Hour), Until: now,
		PullRequests: []readservice.TelemetryCostAggregate{},
		Issues:       []readservice.TelemetryCostAggregate{},
	}}
	var stdout, stderr bytes.Buffer
	code := runCostAt(
		[]string{"--pr", "4398", "--provider", "github", "--window", "1d", "--json", "C:\\instance"},
		&stdout,
		&stderr,
		now,
		func(root string, rebuild bool) (costReader, io.Closer, error) {
			if root != "C:\\instance" || rebuild {
				t.Fatalf("open(%q, %v)", root, rebuild)
			}
			return reader, io.NopCloser(strings.NewReader("")), nil
		},
	)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if reader.request.Scope != readservice.TelemetryCostScopePullRequest ||
		reader.request.ExternalID != "4398" || reader.request.Provider != "github" ||
		!reader.request.Since.Equal(now.Add(-24*time.Hour)) || !reader.request.Until.Equal(now) {
		t.Fatalf("request = %+v", reader.request)
	}
	want := "{\"provider\":\"github\",\"scope\":\"pr\",\"externalId\":\"4398\",\"since\":\"2026-09-06T01:02:03Z\",\"until\":\"2026-09-07T01:02:03Z\",\"pullRequests\":[],\"issues\":[]}\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestCostCommandSupportsIssueAndSummaryHumanOutput(t *testing.T) {
	now := time.Date(2026, 9, 7, 1, 2, 3, 0, time.UTC)
	for _, test := range []struct {
		name      string
		args      []string
		wantScope string
		wantID    string
	}{
		{name: "summary", args: []string{"--window", "7d"}, wantScope: "summary"},
		{name: "issue", args: []string{"--issue", "42", "--window", "7d"}, wantScope: "issue", wantID: "42"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &fakeCostReader{result: readservice.TelemetryCostResult{
				Scope: test.wantScope, ExternalID: test.wantID,
				Since: now.Add(-7 * 24 * time.Hour), Until: now,
				PullRequests: []readservice.TelemetryCostAggregate{{
					Provider: "github", ExternalKind: "pr", ExternalID: "90",
					NativeTotals:     []readservice.TelemetryCostAmount{{Unit: "aiCredits", Value: 2.5}},
					NormalizedTotals: []readservice.TelemetryCostAmount{{Unit: "usd", Value: 0.025, Estimated: true}},
					Coverage: readservice.TelemetryCostCoverage{
						TotalRuns: 2, MeasuredRuns: 1, TotalAttempts: 3, MeasuredAttempts: 2, LowerBound: true,
					},
					Models: []readservice.TelemetryCostModelAggregate{{
						Model: "gpt-5.6-sol", UsageAttempts: 2, MeasuredAttempts: 2,
						NativeTotals: []readservice.TelemetryCostAmount{{Unit: "aiCredits", Value: 2.5}},
					}},
				}},
				Issues: []readservice.TelemetryCostAggregate{},
			}}
			var stdout, stderr bytes.Buffer
			code := runCostAt(test.args, &stdout, &stderr, now, func(string, bool) (costReader, io.Closer, error) {
				return reader, io.NopCloser(strings.NewReader("")), nil
			})
			if code != 0 || stderr.Len() != 0 {
				t.Fatalf("code = %d, stderr = %q", code, stderr.String())
			}
			if reader.request.Scope != test.wantScope || reader.request.ExternalID != test.wantID {
				t.Fatalf("request = %+v", reader.request)
			}
			wantFragments := []string{
				"COST ATTRIBUTION",
				"PR #90 (github)",
				"Native: 2.5000 AI credits",
				"Normalized estimate: $0.0250 estimated",
				"Coverage: lower bound; 1/2 runs, 2/3 attempts measured",
				"Model gpt-5.6-sol: 2.5000 AI credits; 2/2 attempts measured",
			}
			for _, fragment := range wantFragments {
				if !strings.Contains(stdout.String(), fragment) {
					t.Fatalf("stdout %q does not contain %q", stdout.String(), fragment)
				}
			}
		})
	}
}

func TestCostCommandJSONSupportsEveryQueryMode(t *testing.T) {
	now := time.Date(2026, 9, 7, 1, 2, 3, 0, time.UTC)
	for _, test := range []struct {
		args      []string
		wantScope string
		wantID    string
	}{
		{args: []string{"--json"}, wantScope: "summary"},
		{args: []string{"--json", "--pr", "17"}, wantScope: "pr", wantID: "17"},
		{args: []string{"--json", "--issue", "23"}, wantScope: "issue", wantID: "23"},
	} {
		reader := &fakeCostReader{result: readservice.TelemetryCostResult{
			Scope: test.wantScope, ExternalID: test.wantID,
			Since: now.Add(-7 * 24 * time.Hour), Until: now,
			PullRequests: []readservice.TelemetryCostAggregate{},
			Issues:       []readservice.TelemetryCostAggregate{},
		}}
		var stdout, stderr bytes.Buffer
		code := runCostAt(test.args, &stdout, &stderr, now, func(string, bool) (costReader, io.Closer, error) {
			return reader, io.NopCloser(strings.NewReader("")), nil
		})
		if code != 0 || stderr.Len() != 0 || !json.Valid(stdout.Bytes()) {
			t.Fatalf("runCostAt(%v) = %d, stdout %q, stderr %q", test.args, code, stdout.String(), stderr.String())
		}
		if reader.request.Scope != test.wantScope || reader.request.ExternalID != test.wantID {
			t.Fatalf("runCostAt(%v) request = %+v", test.args, reader.request)
		}
	}
}

func TestCostCommandRejectsInvalidSelectionsAndOutputErrors(t *testing.T) {
	now := time.Date(2026, 9, 7, 1, 2, 3, 0, time.UTC)
	open := func(string, bool) (costReader, io.Closer, error) {
		return &fakeCostReader{result: readservice.TelemetryCostResult{
			Scope: "summary", Since: now.Add(-time.Hour), Until: now,
			PullRequests: []readservice.TelemetryCostAggregate{}, Issues: []readservice.TelemetryCostAggregate{},
		}}, io.NopCloser(strings.NewReader("")), nil
	}
	for _, args := range [][]string{
		{"--pr", "1", "--issue", "2"},
		{"--since", "2026-09-01T00:00:00Z", "--window", "1d"},
		{"--window", "91d"},
		{"--since", "2026-09-08T00:00:00Z", "--until", "2026-09-07T00:00:00Z"},
	} {
		var stdout, stderr bytes.Buffer
		if code := runCostAt(args, &stdout, &stderr, now, open); code != 2 || stderr.Len() == 0 {
			t.Fatalf("runCostAt(%v) = %d, stderr %q", args, code, stderr.String())
		}
	}
	var stderr bytes.Buffer
	if code := runCostAt([]string{"--window", "1h"}, costFailWriter{}, &stderr, now, open); code != 2 ||
		!strings.Contains(stderr.String(), "write cost report") {
		t.Fatalf("write failure code = %d, stderr = %q", code, stderr.String())
	}
}
