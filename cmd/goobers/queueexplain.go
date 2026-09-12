package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readservice"
)

const queueExplainHelp = "Usage: goobers queue-explain --gaggle=<name> --workflow=<name> --pr=<number> [--json] [path]\n\n" +
	"Explain one PR using the latest retained selection observation for that exact\n" +
	"workflow. This is historical evidence, not permission to claim or merge.\n" +
	"Reads the daemon's existing projection without rebuilding journal history or\n" +
	"querying providers. An absent PR is unknown, not eligible; reports can be partial\n" +
	"or truncated. Empty --gaggle selects only the legacy unscoped namespace.\n" +
	"Exit codes: 0 = observation displayed, 1 = evidence unavailable/not observed,\n" +
	"2 = usage or I/O error.\n"

type queueEvidenceReader interface {
	QueueEligibility(context.Context, string, string) (readservice.QueueEligibilityView, error)
}

func openQueueEvidence(ctx context.Context, root string) (queueEvidenceReader, io.Closer, error) {
	layout := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		return nil, nil, err
	}
	definitions, validation, err := loadConfigDirectory(layout.ConfigDir())
	if err != nil {
		return nil, nil, err
	}
	store, err := readmodel.OpenExistingReader(ctx, layout.ReadDB())
	if err != nil {
		return nil, nil, err
	}
	reader, err := readservice.NewLocal(readservice.LocalSources{
		Layout: layout, Config: cfg, Definitions: definitions, Validation: validation, ReadModel: store,
	}, func() bool { return true })
	if err != nil {
		_ = store.Close()
		return nil, nil, err
	}
	return reader, store, nil
}

func runQueueExplain(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("queue-explain", flag.ContinueOnError)
	fs.SetOutput(stderr)
	gaggle := fs.String("gaggle", "", "exact gaggle namespace (empty selects legacy only)")
	workflow := fs.String("workflow", "", "exact workflow name (required)")
	pr := fs.Int("pr", 0, "pull request number (required)")
	jsonOutput := fs.Bool("json", false, "emit historical evidence and the requested PR number")
	fs.Usage = helpUsage(stderr, "queue-explain")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *workflow == "" || *pr <= 0 || fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reader, closer, err := openQueueEvidence(ctx, root)
	if err != nil {
		pf(stderr, "error: queue observation unavailable: %v\n", err)
		return 2
	}
	defer func() { _ = closer.Close() }()
	return explainQueueEvidence(ctx, reader, *gaggle, *workflow, *pr, *jsonOutput, stdout, stderr)
}

func explainQueueEvidence(ctx context.Context, reader queueEvidenceReader, gaggle, workflow string, pr int, jsonOutput bool, stdout, stderr io.Writer) int {
	view, err := reader.QueueEligibility(ctx, gaggle, workflow)
	if err != nil {
		pf(stderr, "error: read queue observation: %v\n", err)
		return 2
	}
	found := false
	if view.Status == "observed" && view.Report != nil {
		for _, item := range view.Report.Items {
			if item.Number == pr {
				found = true
				break
			}
		}
	}
	if jsonOutput {
		// Keep the original bounded report intact: filtering items would falsify
		// its matching/omitted accounting. Selection is explicit in this wrapper.
		result := struct {
			RequestedPR int                              `json:"requestedPr"`
			Found       bool                             `json:"found"`
			Evidence    readservice.QueueEligibilityView `json:"evidence"`
		}{pr, found, view}
		err = json.NewEncoder(stdout).Encode(result)
	} else {
		err = writeQueueEvidence(stdout, view, pr)
	}
	if err != nil {
		pf(stderr, "error: write queue observation: %v\n", err)
		return 2
	}
	if !found {
		return 1
	}
	return 0
}

// A zero PR prints the whole bounded observation for status.
func writeQueueEvidence(w io.Writer, view readservice.QueueEligibilityView, pr int) error {
	if _, err := fmt.Fprintf(w, "PR queue %s/%s: historical selection evidence, not permission to claim or merge\n", view.Gaggle, view.Workflow); err != nil {
		return err
	}
	if view.Status != "observed" || view.Report == nil {
		_, err := fmt.Fprintf(w, "  %s: %s\n", view.Status, view.Problem)
		return err
	}
	report := view.Report
	if view.ReadState != nil {
		if _, err := fmt.Fprintf(w, "  projection completeness=%s lag-seconds=%g degraded=%q\n", view.ReadState.Completeness, view.ReadState.LagSeconds, view.ReadState.Degraded); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "  observed %s in run %s; complete snapshot=%t; matching=%d; omitted=%d\n", report.ObservedAt.UTC().Format(time.RFC3339Nano), report.RunID, report.CompleteSnapshot, report.MatchingItems, report.OmittedItems); err != nil {
		return err
	}
	found := false
	for _, item := range report.Items {
		if pr != 0 && item.Number != pr {
			continue
		}
		found = true
		policy := item.Reason
		if item.Eligible {
			policy = "eligible under policy"
		}
		if _, err := fmt.Fprintf(w, "  PR #%d: %q\n    claim=%s owner=%q provider-label=%t comparison=%s\n    next: %q\n    claim next: %q\n", item.Number, policy, item.Claim.State, item.Claim.OwnerRunID, item.Claim.ProviderClaimLabel, item.Claim.Comparison, item.NextStep, item.Claim.NextStep); err != nil {
			return err
		}
		if item.Claim.ExpiresAt != nil {
			if _, err := fmt.Fprintf(w, "    lease expiry: %s\n", item.Claim.ExpiresAt.UTC().Format(time.RFC3339Nano)); err != nil {
				return err
			}
		}
	}
	if pr != 0 && !found {
		_, err := fmt.Fprintf(w, "  PR #%d is not in this retained observation; eligibility is unknown (it may be outside scope, omitted, or not yet observed).\n", pr)
		return err
	}
	return nil
}
