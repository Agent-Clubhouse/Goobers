package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

const telemetryMergesHelp = "Usage: goobers telemetry merges [--json] [--gaggle=name] [--instance-id=id] [--repository-api-url=url] [--compare-github=owner/repository] [--shared-identities=login[,login]] [--since=RFC3339] [--until=RFC3339] [--rebuild] [path]\n\n" +
	"Report confirmed PR landings and daily UTC counts from retained telemetry.\n" +
	"Defaults to the last 7 days; maximum window 90 days and 10000 mutation events.\n" +
	"The interval includes --since and excludes --until. Repeated receipts for\n" +
	"one PR count once; conflicting instance/gaggle/commit claims are excluded.\n" +
	"Legacy merge operations without explicit confirmation are not verified.\n" +
	"Unverified event/conflict counts cover the whole window before filters.\n" +
	"Use --compare-github=owner/repository --shared-identities=login[,login] for\n" +
	"repository-wide forge residuals, independent of display filters. Requires\n" +
	"GOOBERS_CRED_GITHUB_PR_READ with pull-request read permission.\n" +
	"Forge pagination is not an atomic snapshot; unknown mergers stay unknown.\n" +
	"Exit codes: 0 = OK; 2 = usage, query, or output error.\n"

func runTelemetryMerges(args []string, stdout, stderr io.Writer) int {
	return runTelemetryMergesAt(args, stdout, stderr, time.Now())
}

func runTelemetryMergesAt(args []string, stdout, stderr io.Writer, now time.Time) int {
	fs := newCLIFlagSet("telemetry merges", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit merge records and daily counts as JSON")
	gaggle := fs.String("gaggle", "", "filter confirmed records to a gaggle")
	instanceID := fs.String("instance-id", "", "filter confirmed records to an originating instance")
	repository := fs.String("repository-api-url", "", "filter to the canonical repository API address")
	sinceValue := fs.String("since", "", "inclusive start timestamp")
	untilValue := fs.String("until", "", "exclusive end timestamp")
	rebuild := fs.Bool("rebuild", false, "rebuild from retained journals before reading")
	compareRepo := fs.String("compare-github", "", "compare an explicit GitHub owner/repository against retained provenance")
	sharedIdentities := fs.String("shared-identities", "", "comma-separated merger logins treated as same-identity residuals")
	fs.Usage = helpUsage(stderr, "telemetry merges")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	if *instanceID != "" && !instance.ValidIdentity(*instanceID) {
		pf(stderr, "error: invalid --instance-id\n")
		return 2
	}
	if *compareRepo != "" || *sharedIdentities != "" {
		if err := validateMergeComparisonFlags(*compareRepo, *sharedIdentities); err != nil {
			pf(stderr, "error: %v\n", err)
			return 2
		}
	}
	since, until, err := parseTelemetryWindow(*sinceValue, *untilValue)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if until.IsZero() {
		until = now.UTC()
	}
	if since.IsZero() {
		since = until.Add(-7 * 24 * time.Hour)
	}
	if !until.After(since) || until.Sub(since) > 90*24*time.Hour {
		pf(stderr, "error: merge window must be increasing and at most 90 days\n")
		return 2
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}
	db, err := openRollup(instance.NewLayout(root), *rebuild)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	defer func() { _ = db.Close() }()
	report, err := db.MergeProvenance(context.Background(), rollup.MergeReportQuery{Since: since, Until: until, InstanceID: *instanceID, Gaggle: *gaggle, RepositoryAPIURL: *repository})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if *compareRepo != "" {
		report.Comparison, err = compareGitHubMerges(root, *compareRepo, *sharedIdentities, db, rollup.MergeReportQuery{Since: since, Until: until})
		if err != nil {
			pf(stderr, "error: compare merge inventory: %v\n", err)
			return 2
		}
	}
	if *jsonOutput {
		if err := json.NewEncoder(stdout).Encode(report); err != nil {
			pf(stderr, "error: write merge report: %v\n", err)
			return 2
		}
		return 0
	}
	if err := writeMergeReport(stdout, report); err != nil {
		pf(stderr, "error: write merge report: %v\n", err)
		return 2
	}
	return 0
}

func writeMergeReport(output io.Writer, report rollup.MergeReport) error {
	var buffer bytes.Buffer
	stdout := &buffer
	pf(stdout, "Confirmed merges: %d; unverified mutation events: %d; conflicting PRs: %d\n", len(report.Merges), report.UnverifiedMutationEvents, report.ConflictingPullRequests)
	pf(stdout, "UTC DAY\tINSTANCE\tGAGGLE\tPROVIDER\tREPOSITORY API\tMERGES\n")
	for _, row := range report.Daily {
		pf(stdout, "%s\t%s\t%s\t%s\t%s\t%d\n", row.Day, row.InstanceID, row.Gaggle, row.Provider, row.RepositoryAPIURL, row.Count)
	}
	pf(stdout, "Accepted queue entries (not completed merges): %d; unverified queue events: %d; conflicting entries: %d\n", len(report.QueueAdmissions), report.UnverifiedQueueEvents, report.ConflictingQueueEntries)
	for _, entry := range report.QueueAdmissions {
		pf(stdout, "%s\t%s\t%s\tPR %s\t%s\n", entry.InstanceID, entry.Gaggle, entry.RepositoryAPIURL, entry.PullID, entry.EntryID)
	}
	if report.Comparison == nil {
		pf(stdout, "Coverage: retained telemetry only; no forge-inventory comparison.\n")
	} else {
		pf(stdout, "Repository-wide forge comparison (unfiltered; pagination is not an atomic snapshot):\n")
		pf(stdout, "UTC DAY\tCATEGORY\tINSTANCE\tGAGGLE\tMERGES\n")
		for _, row := range report.Comparison.Daily {
			pf(stdout, "%s\t%s\t%s\t%s\t%d\n", row.Day, row.Category, row.InstanceID, row.Gaggle, row.Count)
		}
	}
	_, err := buffer.WriteTo(output)
	return err
}
