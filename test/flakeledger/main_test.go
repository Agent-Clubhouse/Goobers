package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/goobers/goobers/internal/flake"
	"github.com/goobers/goobers/providers"
)

type fakeLedgerProvider struct {
	items         []providers.WorkItem
	ensuredLabels []providers.WorkItemLabel
	creates       []providers.CreateWorkItemRequest
	updates       []providers.UpdateWorkItemRequest
	comments      map[string][]providers.Comment
}

func TestDistinguishingSignatureRejectsOnlyRunnerAndPackageSummaries(t *testing.T) {
	for _, signature := range []string{
		flake.NoStableSignature,
		"coverage: 89.7% of statements",
		"coverage: 89.7% of statements in ./...",
		"coverage: [no statements]",
		"ok github.com/goobers/goobers/internal/bootstrap 120.228s coverage: 89.7% of statements",
		"ok github.com/goobers/goobers/internal/bootstrap (cached) coverage: [no statements]",
		"-test.shuffle=<value> | coverage: 89.7% of statements | ok example/pkg 1.2s",
	} {
		if distinguishingSignature(signature) {
			t.Errorf("distinguishingSignature(%q) = true, want false", signature)
		}
	}
	if !distinguishingSignature("start Temporal dev server: context deadline exceeded | coverage: 89.7% of statements") {
		t.Fatal("underlying failure was rejected with its coverage summary")
	}
	const exact4333Body = "FAIL\ncoverage: 89.7% of statements\nFAIL\tgithub.com/goobers/goobers/internal/bootstrap\t120.228s"
	if signature := flake.NormalizeSignature(exact4333Body); distinguishingSignature(signature) {
		t.Fatalf("normalized #4333 signature %q was accepted as distinguishing", signature)
	}
	if !distinguishingSignature("ok this is meaningful output") {
		t.Fatal("unprefixed meaningful output beginning with ok was rejected")
	}
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
	// All three labels a filed flake needs are ensured, so a fresh repo does
	// not have to be pre-seeded by hand.
	if !slices.Equal(ensuredNames(provider.ensuredLabels),
		[]string{flakeLabel, approvedLabel, cloudLabel}) {
		t.Fatalf("ensured labels = %+v", provider.ensuredLabels)
	}
	if len(provider.updates) != 1 {
		t.Fatalf("updates = %+v", provider.updates)
	}
	update := provider.updates[0]
	// A refresh appends the occurrence and touches nothing else. It must not
	// strip the goobers:* labels the backlog puts on a filed flake — doing so
	// would tear a claim marker off an issue mid-run.
	if update.ID != "7" ||
		len(update.RemoveLabels) != 0 || len(update.AddLabels) != 0 ||
		!strings.Contains(update.Comment, "2 occurrence(s)") || update.State != "" ||
		update.Title != nil || update.Body != nil || update.Milestone != nil {
		t.Fatalf("update = %+v", update)
	}
	if len(provider.creates) != 1 {
		t.Fatalf("creates = %+v", provider.creates)
	}
	create := provider.creates[0]
	if !slices.Equal(create.Labels, []string{flakeLabel, approvedLabel, cloudLabel}) || create.Status != "" ||
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
	// An occurrence already recorded leaves nothing to say, and label stripping
	// is gone, so the whole refresh is a no-op rather than an empty update.
	if result.Refreshed != 0 || len(provider.updates) != 0 {
		t.Fatalf("result=%+v updates=%+v", result, provider.updates)
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
	if result != (publishResult{}) || len(provider.ensuredLabels) != 3 ||
		len(provider.creates) != 0 || len(provider.updates) != 0 {
		t.Fatalf("result=%+v provider=%+v", result, provider)
	}
}

// A signature that is only the test runner echoing its own flags names no
// failure, so filing an issue for it would file a fresh issue every run
// instead of maintaining one issue for one defect (#4221).
func TestPublishRefusesIssueWithoutDistinguishingSignature(t *testing.T) {
	t.Parallel()
	echoOnly := strings.Repeat("f", 64)
	real := strings.Repeat("a", 64)
	provider := &fakeLedgerProvider{}
	report := failuresReport{
		SchemaVersion: stressSchema,
		Run:           runMetadata{RunID: "123"},
		Failures: []testFailure{
			seedFailure(echoOnly, "-test.shuffle <value>"),
			seedFailure(real, "Resume() = 3, want 4 | -test.shuffle <value>"),
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
	if result != (publishResult{Created: 1, Skipped: 1}) {
		t.Fatalf("result = %+v", result)
	}
	if len(provider.creates) != 1 || provider.creates[0].RunID != "flake-"+real {
		t.Fatalf("creates = %+v", provider.creates)
	}
}

// Closing a flake issue must not retire its fingerprint (#4612). Before this,
// the recurrence commented into a closed issue nobody watches and no new issue
// was filed either, so the signal was lost in both directions.
func TestPublishReopensClosedIssueOnRecurrence(t *testing.T) {
	t.Parallel()
	fingerprint := strings.Repeat("a", 64)
	provider := &fakeLedgerProvider{items: []providers.WorkItem{{
		ID:     "7",
		Body:   fingerprintMarker(fingerprint),
		Labels: []string{flakeLabel, approvedLabel, cloudLabel},
		State:  "closed",
	}}}
	report := failuresReport{
		SchemaVersion: stressSchema,
		Run:           runMetadata{RunID: "456", URL: "https://github.com/acme/app/actions/runs/456"},
		Failures:      []testFailure{seedFailure(fingerprint, "Resume() = 3, want 4")},
	}
	result, err := publish(context.Background(), provider, providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    "acme",
		Name:     "app",
	}, report)
	if err != nil {
		t.Fatal(err)
	}
	if result != (publishResult{Reopened: 1}) || len(provider.creates) != 0 || len(provider.updates) != 1 {
		t.Fatalf("result=%+v creates=%+v updates=%+v", result, provider.creates, provider.updates)
	}
	update := provider.updates[0]
	if update.ID != "7" || update.State != stateOpen {
		t.Fatalf("update = %+v", update)
	}
	// A reopen must be distinguishable from an ordinary recurrence and must
	// name the run that reopened it, since it means a landed fix regressed.
	if !strings.Contains(update.Comment, "reopened") ||
		!strings.Contains(update.Comment, "regression") ||
		!strings.Contains(update.Comment, "`123`") {
		t.Fatalf("reopen comment = %q", update.Comment)
	}
	// Reopening must not disturb the labels an operator left on the issue.
	if len(update.AddLabels) != 0 || len(update.RemoveLabels) != 0 {
		t.Fatalf("reopen changed labels: %+v", update)
	}
}

// An occurrence already recorded on a closed issue means the operator closed it
// after that occurrence landed. Reopening then would fight the operator.
func TestPublishDoesNotReopenClosedIssueForRecordedOccurrence(t *testing.T) {
	t.Parallel()
	fingerprint := strings.Repeat("b", 64)
	report := failuresReport{
		SchemaVersion: stressSchema,
		Run:           runMetadata{RunID: "456"},
		Failures:      []testFailure{seedFailure(fingerprint, "Resume() = 3, want 4")},
	}
	provider := &fakeLedgerProvider{
		items: []providers.WorkItem{{
			ID:     "7",
			Body:   fingerprintMarker(fingerprint),
			Labels: []string{flakeLabel},
			State:  "closed",
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
	if result != (publishResult{}) || len(provider.updates) != 0 {
		t.Fatalf("result=%+v updates=%+v", result, provider.updates)
	}
}

// Only an observed failure reopens anything: a green run reports no failures,
// so a closed issue stays closed and is never even updated.
func TestPublishGreenRunLeavesClosedIssueUntouched(t *testing.T) {
	t.Parallel()
	provider := &fakeLedgerProvider{items: []providers.WorkItem{{
		ID:     "7",
		Body:   fingerprintMarker(strings.Repeat("c", 64)),
		Labels: []string{flakeLabel},
		State:  "closed",
	}}}
	result, err := publish(context.Background(), provider, providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    "acme",
		Name:     "app",
	}, failuresReport{SchemaVersion: stressSchema, Run: runMetadata{RunID: "456"}})
	if err != nil {
		t.Fatal(err)
	}
	if result != (publishResult{}) || len(provider.updates) != 0 || len(provider.creates) != 0 {
		t.Fatalf("result=%+v updates=%+v creates=%+v", result, provider.updates, provider.creates)
	}
}

// An open issue's recurrence keeps its existing behavior: comment only, with no
// state change and no reopen wording.
func TestPublishOpenIssueRecurrenceStillCommentsOnly(t *testing.T) {
	t.Parallel()
	fingerprint := strings.Repeat("d", 64)
	provider := &fakeLedgerProvider{items: []providers.WorkItem{{
		ID:     "7",
		Body:   fingerprintMarker(fingerprint),
		Labels: []string{flakeLabel},
		State:  "open",
	}}}
	result, err := publish(context.Background(), provider, providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    "acme",
		Name:     "app",
	}, failuresReport{
		SchemaVersion: stressSchema,
		Run:           runMetadata{RunID: "456"},
		Failures:      []testFailure{seedFailure(fingerprint, "Resume() = 3, want 4")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result != (publishResult{Refreshed: 1}) || len(provider.updates) != 1 {
		t.Fatalf("result=%+v updates=%+v", result, provider.updates)
	}
	if provider.updates[0].State != "" || strings.Contains(provider.updates[0].Comment, "reopened") {
		t.Fatalf("update = %+v", provider.updates[0])
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

// TestFlakeLedgerPublishersShareConcurrencyGroup guards against the
// duplicate-issue race this ledger is exposed to: publish() lists existing
// flake issues then creates one for any fingerprint it didn't find, which is
// not atomic. Every workflow publishing to the repo-wide issue tracker must
// therefore use the same fixed, ref-independent concurrency group.
func TestFlakeLedgerPublishersShareConcurrencyGroup(t *testing.T) {
	t.Parallel()
	type concurrency struct {
		Group string `yaml:"group"`
	}
	var stress struct {
		Jobs map[string]struct {
			Concurrency concurrency `yaml:"concurrency"`
		} `yaml:"jobs"`
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "stress.yml"))
	if err != nil {
		t.Fatalf("read stress.yml: %v", err)
	}
	if err := yaml.Unmarshal(raw, &stress); err != nil {
		t.Fatalf("parse stress.yml: %v", err)
	}
	job, ok := stress.Jobs["flake-ledger"]
	if !ok {
		t.Fatal("stress.yml has no flake-ledger job")
	}
	group := job.Concurrency.Group
	if group == "" {
		t.Fatal("flake-ledger job has no concurrency group; concurrent publishers can race on the same list-then-create sequence")
	}

	var watch struct {
		Concurrency concurrency `yaml:"concurrency"`
	}
	raw, err = os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "flake-watch.yml"))
	if err != nil {
		t.Fatalf("read flake-watch.yml: %v", err)
	}
	if err := yaml.Unmarshal(raw, &watch); err != nil {
		t.Fatalf("parse flake-watch.yml: %v", err)
	}
	if watch.Concurrency.Group != group {
		t.Fatalf("flake-watch concurrency group = %q, want shared ledger publisher group %q", watch.Concurrency.Group, group)
	}
	for _, refExpr := range []string{"github.ref", "github.event.pull_request.number", "github.run_id", "github.head_ref"} {
		if strings.Contains(group, refExpr) {
			t.Fatalf("ledger publisher concurrency group %q is scoped by %s; publishers on different refs would not be serialized against each other", group, refExpr)
		}
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

// ensuredNames projects the ensured label set to its names, in call order.
func ensuredNames(labels []providers.WorkItemLabel) []string {
	names := make([]string, 0, len(labels))
	for _, label := range labels {
		names = append(names, label.Name)
	}
	return names
}

// buildBreakFailure is one downstream package's report of a shared compile
// break: its own package, test, wrapper message and relative path to the
// broken file, and so its own per-package fingerprint.
func buildBreakFailure(index int, sha, undefined string) testFailure {
	pkg := fmt.Sprintf("./internal/consumer%02d", index)
	test := fmt.Sprintf("TestConsumer%02d", index)
	text := strings.Join([]string{
		fmt.Sprintf("    consumer_test.go:%d: build consumer %d: exit status 1", 20+index, index),
		"        # github.com/goobers/goobers/cmd/goobers",
		fmt.Sprintf("        %scmd/goobers/root.go:%d:7: undefined: %s", strings.Repeat("../", 1+index%3), 300+index%2, undefined),
	}, "\n")
	signature := flake.NormalizeSignature(text)
	return testFailure{
		Fingerprint:      flake.Fingerprint(pkg, test, signature),
		Package:          pkg,
		Test:             test,
		FailureSignature: signature,
		FailureText:      text,
		LastSeenRun:      "555",
		LastSeenAt:       time.Date(2026, 9, 1, 10, 33, 46+index, 0, time.UTC),
		Occurrences:      1,
		SHA:              sha,
	}
}

func TestPublishGroupsOneBuildBreakAcrossThirteenPackagesIntoOneIssue(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	var failures []testFailure
	for index := range 13 {
		failures = append(failures, buildBreakFailure(index, "", "newRegistry"))
	}
	// Two of the per-package issues were already filed (by a ledger that did
	// not group); the grouped issue supersedes them.
	provider := &fakeLedgerProvider{items: []providers.WorkItem{
		{ID: "4128", State: stateOpen, Body: fingerprintMarker(failures[0].Fingerprint)},
		{ID: "4129", State: stateOpen, Body: fingerprintMarker(failures[1].Fingerprint)},
		{ID: "4130", State: stateClosed, Body: fingerprintMarker(failures[2].Fingerprint)},
	}}
	report := failuresReport{
		SchemaVersion: stressSchema,
		Run:           runMetadata{RunID: "555", RunAttempt: "1", SHA: sha},
		Failures:      failures,
	}
	// Each package's failure has its own per-package fingerprint, which is
	// what used to fan this out into 13 issues.
	if err := validateReport(report); err != nil {
		t.Fatal(err)
	}
	result, err := publish(context.Background(), provider, providers.RepositoryRef{Owner: "acme", Name: "app"}, report)
	if err != nil {
		t.Fatal(err)
	}
	if len(provider.creates) != 1 || result.Created != 1 {
		t.Fatalf("13 packages broken by one commit filed %d issue(s), want 1: %+v", len(provider.creates), provider.creates)
	}
	create := provider.creates[0]
	signature, _ := flake.BuildBreakSignature(failures[0].FailureText)
	if want := fingerprintMarker(flake.BuildBreakFingerprint(sha, signature)); !strings.HasPrefix(create.Body, want) {
		t.Fatalf("grouped issue is not keyed on commit + compile signature:\n%s", create.Body)
	}
	if !strings.Contains(create.Title, "build break at 0123456789ab in 13 package(s)") {
		t.Fatalf("grouped title = %q", create.Title)
	}
	for _, failure := range failures {
		if !strings.Contains(create.Body, "`"+failure.Package+"` `"+failure.Test+"`") {
			t.Fatalf("grouped issue does not list affected package %s:\n%s", failure.Package, create.Body)
		}
	}
	if !strings.Contains(create.Body, "- #4128 (superseded") || !strings.Contains(create.Body, "- #4129 (superseded") ||
		strings.Contains(create.Body, "#4130") {
		t.Fatalf("grouped issue does not cross-link exactly the open superseded issues:\n%s", create.Body)
	}
	// The open per-package issues are closed as duplicates of the grouped
	// one; the already-closed one is left alone and nothing is refreshed.
	if result.Superseded != 2 || result.Refreshed != 0 || result.Reopened != 0 || len(provider.updates) != 2 {
		t.Fatalf("result = %+v, updates = %+v", result, provider.updates)
	}
	for index, id := range []string{"4128", "4129"} {
		update := provider.updates[index]
		if update.ID != id || update.State != stateClosed || !strings.Contains(update.Comment, "Duplicate of #99.") ||
			update.Body == nil || !strings.HasSuffix(*update.Body, supersededMarker("99")) {
			t.Fatalf("update %d = %+v, want %s closed as a duplicate of #99 and marked superseded", index, update, id)
		}
	}
	if !strings.Contains(create.Body, buildBreakMarker) {
		t.Fatalf("grouped issue is not tagged as a build break:\n%s", create.Body)
	}

	// Re-publishing the same report is a no-op: the grouped issue already
	// carries this occurrence and its superseded issues are already closed.
	provider.items = []providers.WorkItem{
		{ID: "99", State: stateOpen, Body: create.Body},
		{ID: "4128", State: stateClosed, Body: *provider.updates[0].Body},
		{ID: "4129", State: stateClosed, Body: *provider.updates[1].Body},
		{ID: "4130", State: stateClosed, Body: fingerprintMarker(failures[2].Fingerprint)},
	}
	provider.creates, provider.updates = nil, nil
	again, err := publish(context.Background(), provider, providers.RepositoryRef{Owner: "acme", Name: "app"}, report)
	if err != nil {
		t.Fatal(err)
	}
	if len(provider.creates) != 0 || len(provider.updates) != 0 || again != (publishResult{}) {
		t.Fatalf("re-publish result = %+v, creates = %d, updates = %+v", again, len(provider.creates), provider.updates)
	}

	// Later, one superseded package recurs on its own (no group to join).
	// The recurrence lands on the grouped issue, not on the closed duplicate,
	// and an operator's reopen of the duplicate is not overruled.
	provider.items[1].State = stateOpen
	provider.creates, provider.updates = nil, nil
	recurrence := failures[0]
	recurrence.LastSeenRun = "777"
	later, err := publish(context.Background(), provider, providers.RepositoryRef{Owner: "acme", Name: "app"}, failuresReport{
		SchemaVersion: stressSchema,
		Run:           runMetadata{RunID: "777", RunAttempt: "1", SHA: "fedcba9876543210fedcba9876543210fedcba98"},
		Failures:      []testFailure{recurrence},
	})
	if err != nil {
		t.Fatal(err)
	}
	if later.Refreshed != 1 || later.Superseded != 0 || len(provider.creates) != 0 ||
		len(provider.updates) != 1 || provider.updates[0].ID != "99" || provider.updates[0].State != "" {
		t.Fatalf("recurrence result = %+v, updates = %+v; want one refresh of grouped #99", later, provider.updates)
	}
}

func TestPublishGroupsHeaderOnlyBuildBreakFromIncident(t *testing.T) {
	// The #4128..#4140 shape: each consumer reports its own wrapper and only
	// the go tool's header for the broken package.
	var failures []testFailure
	for index := range 13 {
		pkg := fmt.Sprintf("./consumer%02d", index)
		text := fmt.Sprintf("main_test.go:%d: build tool %d: exit status 1\n# github.com/goobers/goobers/cmd/goobers\nFAIL", 90+index, index)
		signature := flake.NormalizeSignature(text)
		failures = append(failures, testFailure{
			Fingerprint:      flake.Fingerprint(pkg, "TestBuild", signature),
			Package:          pkg,
			Test:             "TestBuild",
			FailureSignature: signature,
			FailureText:      text,
			LastSeenRun:      "1",
			LastSeenAt:       time.Date(2026, 9, 1, 10, 33, 46, 0, time.UTC),
			Occurrences:      1,
		})
	}
	provider := &fakeLedgerProvider{}
	result, err := publish(context.Background(), provider, providers.RepositoryRef{Owner: "acme", Name: "app"}, failuresReport{
		SchemaVersion: stressSchema,
		Run:           runMetadata{RunID: "1", SHA: "0123456789abcdef0123456789abcdef01234567"},
		Failures:      failures,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != 1 || !strings.Contains(provider.creates[0].Title, "in 13 package(s): build failed: # github.com/goobers/goobers/cmd/goobers") {
		t.Fatalf("result = %+v, creates = %+v; want one grouped issue for the cmd/goobers break", result, provider.creates)
	}
}

func TestPublishKeepsDifferentBuildBreaksSeparate(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	failures := []testFailure{
		// Same commit, different compile errors.
		buildBreakFailure(0, sha, "newRegistry"),
		buildBreakFailure(1, sha, "newRegistry"),
		buildBreakFailure(2, sha, "loadPolicy"),
		buildBreakFailure(3, sha, "loadPolicy"),
		// Same compile error, different commits.
		buildBreakFailure(4, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "newRegistry"),
		buildBreakFailure(5, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "newRegistry"),
		// A compile error only one package saw keeps its own identity.
		buildBreakFailure(6, "cccccccccccccccccccccccccccccccccccccccc", "newRegistry"),
		// Not a build break at all.
		seedFailure(strings.Repeat("d", 64), "assertion failed"),
	}
	provider := &fakeLedgerProvider{}
	result, err := publish(context.Background(), provider, providers.RepositoryRef{Owner: "acme", Name: "app"}, failuresReport{
		SchemaVersion: stressSchema,
		Run:           runMetadata{RunID: "1", SHA: "ffffffffffffffffffffffffffffffffffffffff"},
		Failures:      failures,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != 5 || len(provider.creates) != 5 {
		t.Fatalf("created %d issue(s), want 5 (3 groups + 2 ungrouped): %+v", len(provider.creates), provider.creates)
	}
	var grouped, single int
	for _, create := range provider.creates {
		if strings.Contains(create.Title, "build break at") {
			grouped++
			if !strings.Contains(create.Title, "in 2 package(s)") {
				t.Fatalf("grouped title = %q, want two packages", create.Title)
			}
			continue
		}
		single++
	}
	if grouped != 3 || single != 2 {
		t.Fatalf("filed %d grouped and %d per-package issue(s), want 3 and 2", grouped, single)
	}
	if !strings.Contains(provider.creates[0].Title, "undefined: newRegistry") ||
		!strings.Contains(provider.creates[1].Title, "undefined: loadPolicy") {
		t.Fatalf("grouped issues lost report order or their signatures: %q, %q", provider.creates[0].Title, provider.creates[1].Title)
	}
}

func TestPublishDoesNotGroupBuildBreaksWithoutACommit(t *testing.T) {
	provider := &fakeLedgerProvider{}
	result, err := publish(context.Background(), provider, providers.RepositoryRef{Owner: "acme", Name: "app"}, failuresReport{
		SchemaVersion: stressSchema,
		Run:           runMetadata{RunID: "1"},
		Failures:      []testFailure{buildBreakFailure(0, "", "newRegistry"), buildBreakFailure(1, "", "newRegistry")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != 2 {
		t.Fatalf("created %d, want each failure filed on its own when no commit names the root cause", result.Created)
	}
}
