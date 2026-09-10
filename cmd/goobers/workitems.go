package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"strings"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readservice"
)

const workItemsHelp = "Usage: goobers work-items [--provider=<name>] [--repository=<owner/name>] [--kind=<pr|issue>] [--id=<id>] [--limit=<n>] [--json] [--rebuild] [path]\n\n" +
	"List pull requests and issues changed by recorded provider mutations. Use\n" +
	"--id with --provider, --repository, and --kind to show one item.\n" +
	"Exit codes: 0 = OK, 2 = usage, query, or I/O error.\n"

type workItemReader interface {
	WorkItems(context.Context, readservice.WorkItemListOptions) (readservice.WorkItemPage, error)
	WorkItem(context.Context, string, string, string, string) (readservice.WorkItemDetail, error)
}

func runWorkItems(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("work-items", flag.ContinueOnError)
	fs.SetOutput(stderr)
	provider := fs.String("provider", "", "filter to one provider")
	repository := fs.String("repository", "", "repository identity, for example owner/name")
	kind := fs.String("kind", "", "filter to pr or issue")
	externalID := fs.String("id", "", "show one work item's action timeline")
	limit := fs.Int("limit", 100, "maximum work items to return (1-200)")
	jsonOutput := fs.Bool("json", false, "emit JSON")
	rebuild := fs.Bool("rebuild", false, "force a full telemetry rebuild before querying")
	fs.Usage = helpUsage(stderr, "work-items")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 || *limit <= 0 || *limit > readservice.MaxWorkItemsPageSize {
		fs.Usage()
		return 2
	}
	*provider = strings.TrimSpace(*provider)
	*repository = strings.Trim(strings.TrimSpace(*repository), "/")
	*kind = strings.TrimSpace(*kind)
	*externalID = strings.TrimSpace(*externalID)
	if *kind != "" && *kind != "pr" && *kind != "issue" {
		pf(stderr, "error: --kind must be pr or issue\n")
		return 2
	}
	if *externalID != "" && (*provider == "" || *repository == "" || *kind == "") {
		pf(stderr, "error: --id requires --provider, --repository, and --kind\n")
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
	reader, err := readservice.NewTelemetry(db)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	if *externalID != "" {
		return writeWorkItem(reader, *provider, *repository, *kind, *externalID, *jsonOutput, stdout, stderr)
	}

	page, err := reader.WorkItems(context.Background(), readservice.WorkItemListOptions{
		Provider: *provider,
		Kind:     *kind,
		Limit:    *limit,
	})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	return writeWorkItems(page, *jsonOutput, stdout, stderr)
}

func writeWorkItem(
	reader workItemReader,
	provider, repository, kind, externalID string,
	jsonOutput bool,
	stdout, stderr io.Writer,
) int {
	item, err := reader.WorkItem(context.Background(), provider, repository, kind, externalID)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if jsonOutput {
		if err := json.NewEncoder(stdout).Encode(item); err != nil {
			pf(stderr, "error: encode work item: %v\n", err)
			return 2
		}
		return 0
	}
	pf(stdout, "%s#%s (%s %s)\n", item.Repository, item.ExternalID, item.Provider, strings.ToUpper(item.Kind))
	for _, action := range item.Actions {
		pf(stdout, "%s  %-18s  %s/%s  %s\n",
			action.OccurredAt.Format("2006-01-02 15:04:05Z"),
			action.Operation,
			action.Gaggle,
			action.Workflow,
			action.RunID,
		)
	}
	return 0
}

func writeWorkItems(page readservice.WorkItemPage, jsonOutput bool, stdout, stderr io.Writer) int {
	if jsonOutput {
		if err := json.NewEncoder(stdout).Encode(page); err != nil {
			pf(stderr, "error: encode work items: %v\n", err)
			return 2
		}
		return 0
	}
	if len(page.Items) == 0 {
		pln(stdout, "No recorded work-item actions.")
		return 0
	}
	for _, item := range page.Items {
		pf(stdout, "%-6s %-10s %-12s %-18s %s\n",
			strings.ToUpper(item.Kind),
			item.Provider,
			"#"+item.ExternalID,
			item.LastOperation,
			item.URL,
		)
	}
	if page.HasMore {
		pln(stdout, "More work items are available; increase --limit or narrow the filters.")
	}
	return 0
}
