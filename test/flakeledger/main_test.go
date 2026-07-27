package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
)

type fakeLedgerProvider struct {
	items         []providers.WorkItem
	ensuredLabels []providers.WorkItemLabel
	creates       []providers.CreateWorkItemRequest
	updates       []providers.UpdateWorkItemRequest
	comments      map[string][]providers.Comment
}

func (f *fakeLedgerProvider) EnsureWorkItemLabels(
	_ context.Context,
	_ providers.RepositoryRef,
	labels []providers.WorkItemLabel,
) (providers.EnsureWorkItemLabelsResult, error) {
	f.ensuredLabels = append(f.ensuredLabels, labels...)
	return providers.EnsureWorkItemLabelsResult{}, nil
}

func (f *fakeLedgerProvider) ListWorkItems(
	_ context.Context,
	_ providers.ListWorkItemsRequest,
) ([]providers.WorkItem, error) {
	return append([]providers.WorkItem(nil), f.items...), nil
}

func (f *fakeLedgerProvider) ListComments(
	_ context.Context,
	_ providers.RepositoryRef,
	id string,
) ([]providers.Comment, error) {
	return append([]providers.Comment(nil), f.comments[id]...), nil
}

func (f *fakeLedgerProvider) CreateWorkItem(
	_ context.Context,
	req providers.CreateWorkItemRequest,
) (providers.WorkItem, error) {
	f.creates = append(f.creates, req)
	return providers.WorkItem{ID: "99", Title: req.Title, Body: req.Body, Labels: req.Labels}, nil
}

func (f *fakeLedgerProvider) UpdateWorkItem(
	_ context.Context,
	req providers.UpdateWorkItemRequest,
) (providers.WorkItem, error) {
	f.updates = append(f.updates, req)
	return providers.WorkItem{ID: req.ID}, nil
}

func TestRunPublishesSeededFailureAndRefreshesKnownFingerprint(t *testing.T) {
	known := strings.Repeat("a", 64)
	fresh := strings.Repeat("b", 64)
	provider := &fakeLedgerProvider{items: []providers.WorkItem{{
		ID:     "7",
		Body:   fingerprintMarker(known),
		Labels: []string{flakeLabel, "goobers:ready", "goobers/status:claimed", "area:hygiene"},
	}}}
	report := failuresReport{
		SchemaVersion: stressSchema,
		Run: runMetadata{
			RunID:      "123",
			RunAttempt: "2",
			URL:        "https://github.com/acme/app/actions/runs/123",
		},
		Failures: []testFailure{
			seedFailure(known, "known assertion"),
			seedFailure(fresh, "new assertion"),
		},
	}
	input := writeReport(t, report)
	values := map[string]string{
		"GITHUB_TOKEN":      "token",
		"GITHUB_REPOSITORY": "acme/app",
		"GITHUB_API_URL":    "https://github.example/api/v3",
	}
	var stdout, stderr bytes.Buffer
	code := run(
		[]string{"-input", input},
		&stdout,
		&stderr,
		func(name string) string { return values[name] },
		func(token, apiURL string) ledgerProvider {
			if token != "token" || apiURL != values["GITHUB_API_URL"] {
				t.Fatalf("provider factory = token %q, API %q", token, apiURL)
			}
			return provider
		},
	)
	if code != 0 {
		t.Fatalf("run() = %d\nstdout:\n%s\nstderr:\n%s", code, &stdout, &stderr)
	}
	if stdout.String() != "flake ledger: 1 created, 1 refreshed\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if len(provider.ensuredLabels) != 1 || provider.ensuredLabels[0].Name != flakeLabel {
		t.Fatalf("ensured labels = %+v", provider.ensuredLabels)
	}
	if len(provider.updates) != 1 {
		t.Fatalf("updates = %+v", provider.updates)
	}
	update := provider.updates[0]
	if update.ID != "7" ||
		!slices.Equal(update.RemoveLabels, []string{"goobers/status:claimed", "goobers:ready"}) ||
		!strings.Contains(update.Comment, "2 occurrence(s)") || update.State != "" ||
		update.Title != nil || update.Body != nil || update.Milestone != nil {
		t.Fatalf("update = %+v", update)
	}
	if len(provider.creates) != 1 {
		t.Fatalf("creates = %+v", provider.creates)
	}
	create := provider.creates[0]
	if !slices.Equal(create.Labels, []string{flakeLabel}) || create.Status != "" ||
		!strings.Contains(create.Body, fingerprintMarker(fresh)) ||
		!strings.Contains(create.Body, "stress run 123") ||
		!strings.Contains(create.Body, "new assertion") ||
		create.RunID != "flake-"+fresh {
		t.Fatalf("create = %+v", create)
	}
}

func TestPublishBoundsSeededLongFailureAndContinues(t *testing.T) {
	t.Parallel()
	known := strings.Repeat("c", 64)
	freshLong := strings.Repeat("d", 64)
	subsequent := strings.Repeat("e", 64)
	longSignature := strings.Repeat("assertion output ", 8*1024)
	provider := &fakeLedgerProvider{items: []providers.WorkItem{{
		ID:     "7",
		Body:   fingerprintMarker(known),
		Labels: []string{flakeLabel},
	}}}
	report := failuresReport{
		SchemaVersion: stressSchema,
		Run:           runMetadata{RunID: "123"},
		Failures: []testFailure{
			seedFailure(known, longSignature),
			seedFailure(freshLong, longSignature),
			seedFailure(subsequent, "subsequent failure"),
		},
	}
	result, err := publish(context.Background(), provider, providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    "acme",
		Name:     "app",
	}, report)
	if err != nil {
		t.Fatal(err)
	}
	if result != (publishResult{Created: 2, Refreshed: 1}) ||
		len(provider.updates) != 1 || len(provider.creates) != 2 {
		t.Fatalf("result=%+v creates=%d updates=%d", result, len(provider.creates), len(provider.updates))
	}
	if strings.Contains(provider.updates[0].Comment, longSignature) {
		t.Fatal("occurrence comment contains the unbounded signature")
	}
	if strings.Contains(provider.creates[0].Body, longSignature) {
		t.Fatal("issue body contains the unbounded signature")
	}
	if got := len([]rune(renderedSignature(longSignature))); got != signatureLimit {
		t.Fatalf("rendered signature length = %d, want %d", got, signatureLimit)
	}
	if len(provider.updates[0].Comment) >= 64*1024 {
		t.Fatalf("occurrence comment length = %d, want below GitHub limit", len(provider.updates[0].Comment))
	}
	if len(provider.creates[0].Body) >= 64*1024 {
		t.Fatalf("issue body length = %d, want below GitHub limit", len(provider.creates[0].Body))
	}
	if provider.creates[1].RunID != "flake-"+subsequent {
		t.Fatalf("subsequent create = %+v", provider.creates[1])
	}
}

func TestPublishDoesNotDuplicateRecordedOccurrence(t *testing.T) {
	t.Parallel()
	fingerprint := strings.Repeat("d", 64)
	report := failuresReport{
		SchemaVersion: stressSchema,
		Run:           runMetadata{RunID: "123", RunAttempt: "2"},
		Failures:      []testFailure{seedFailure(fingerprint, "known assertion")},
	}
	provider := &fakeLedgerProvider{
		items: []providers.WorkItem{{
			ID:     "7",
			Body:   fingerprintMarker(fingerprint),
			Labels: []string{flakeLabel, "goobers/status:claimed"},
		}},
		comments: map[string][]providers.Comment{
			"7": {{Body: occurrenceMarker(report.Run, report.Failures[0])}},
		},
	}
	result, err := publish(context.Background(), provider, providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    "acme",
		Name:     "app",
	}, report)
	if err != nil {
		t.Fatal(err)
	}
	if result.Refreshed != 1 || len(provider.updates) != 1 {
		t.Fatalf("result=%+v updates=%+v", result, provider.updates)
	}
	update := provider.updates[0]
	if update.Comment != "" || !slices.Equal(update.RemoveLabels, []string{"goobers/status:claimed"}) {
		t.Fatalf("update = %+v", update)
	}
}

func TestPublishGreenRunStillEnsuresFlakeLabel(t *testing.T) {
	t.Parallel()
	provider := &fakeLedgerProvider{}
	result, err := publish(context.Background(), provider, providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    "acme",
		Name:     "app",
	}, failuresReport{SchemaVersion: stressSchema})
	if err != nil {
		t.Fatal(err)
	}
	if result != (publishResult{}) || len(provider.ensuredLabels) != 1 ||
		len(provider.creates) != 0 || len(provider.updates) != 0 {
		t.Fatalf("result=%+v provider=%+v", result, provider)
	}
}

func TestLoadFailuresRejectsMalformedReports(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		report failuresReport
		want   string
	}{
		{name: "schema", report: failuresReport{SchemaVersion: "v0"}, want: "unsupported schema"},
		{
			name: "fingerprint",
			report: failuresReport{
				SchemaVersion: stressSchema,
				Failures:      []testFailure{seedFailure("short", "signature")},
			},
			want: "invalid fingerprint",
		},
		{
			name: "signature",
			report: failuresReport{
				SchemaVersion: stressSchema,
				Failures:      []testFailure{seedFailure(strings.Repeat("a", 64), "")},
			},
			want: "normalized signature",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := loadFailures(writeReport(t, test.report))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("loadFailures() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestIndexIssuesRejectsDuplicateFingerprint(t *testing.T) {
	t.Parallel()
	fingerprint := strings.Repeat("c", 64)
	_, err := indexIssues([]providers.WorkItem{
		{ID: "1", Body: fingerprintMarker(fingerprint)},
		{ID: "2", Body: fingerprintMarker(fingerprint)},
	})
	if err == nil || !strings.Contains(err.Error(), "issues 1 and 2") {
		t.Fatalf("indexIssues() error = %v", err)
	}
}

func TestRunRequiresCredentialsAndRepository(t *testing.T) {
	t.Parallel()
	for _, values := range []map[string]string{
		{"GITHUB_REPOSITORY": "acme/app"},
		{"GITHUB_TOKEN": "token", "GITHUB_REPOSITORY": "invalid"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(nil, &stdout, &stderr, func(name string) string { return values[name] }, nil)
		if code != 2 || stderr.Len() == 0 {
			t.Fatalf("values=%v code=%d stderr=%q", values, code, stderr.String())
		}
	}
}

func seedFailure(fingerprint, signature string) testFailure {
	return testFailure{
		Fingerprint:      fingerprint,
		Package:          "./internal/runner",
		Test:             "TestResume",
		FailureSignature: signature,
		FailureText:      "seeded failure: " + signature,
		LastSeenRun:      "123",
		LastSeenAt:       time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC),
		Occurrences:      2,
	}
}

func writeReport(t *testing.T, report failuresReport) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "failures.json")
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
