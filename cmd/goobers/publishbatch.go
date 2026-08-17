package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/decomposition"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

const publishBatchHelp = "Usage: goobers publish-batch [path]\n\n" +
	"publish-batch is the decomposition workflow's deterministic publication\n" +
	"stage (docs/design/decomposition-workflow.md §5.2-5.3, DEC-4). It consumes\n" +
	"a validated decomposition plan, resumes the prepare/publish protocol by\n" +
	"stable markers, verifies the complete batch, commits eligibility through\n" +
	"the parent published record, and releases the parent claim.\n\n" +
	"Exit codes: 0 = batch published / 1 = business or provider error / 2 = usage error.\n"

type publishBatchResult struct {
	ParentID   string   `json:"parentId"`
	PlanDigest string   `json:"planDigest"`
	ChildIDs   []string `json:"childIds"`
}

type publishBatchProvider interface {
	decomposition.PublisherProvider
}

func runPublishBatch(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("publish-batch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "publish-batch")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	pathArg := ""
	if fs.NArg() == 1 {
		pathArg = fs.Arg(0)
	}
	root := providerStageRoot(pathArg)
	plan, err := readValidatePlanInput[decomposition.Plan](providerInput("planFile", "plan.json"))
	if err != nil {
		pf(stderr, "error: read plan: %v\n", err)
		return 1
	}
	validation, err := readValidatePlanInput[validatePlanResult](providerInput("validationFile", "plan-validation.json"))
	if err != nil {
		pf(stderr, "error: read plan validation: %v\n", err)
		return 1
	}
	if !validation.Valid {
		pln(stderr, "error: refusing to publish a plan that validate-plan did not mark valid")
		return 1
	}
	planDigest, err := decomposition.PlanDigest(plan)
	if err != nil {
		pf(stderr, "error: digest plan: %v\n", err)
		return 1
	}
	if validation.PlanDigest == "" || validation.PlanDigest != planDigest {
		pln(stderr, "error: plan does not match the artifact validate-plan marked valid")
		return 1
	}
	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	var provider publishBatchProvider
	switch repo.Provider {
	case providers.ProviderADO:
		provider, err = newADOProviderForStage(root, repo)
	case providers.ProviderGitea:
		var token string
		token, err = providerToken(capability.GitHubIssuesWrite)
		if err == nil {
			provider, err = newGiteaProviderForStage(root, repo, token, providers.WithGiteaMutationRecorder(sidecarMutationRecorder{kind: "issue"}))
		}
	case providers.ProviderGitHub:
		var token string
		token, err = providerToken(capability.GitHubIssuesWrite)
		if err == nil {
			provider = newCachedGitHubProvider(root, token, providers.WithMutationRecorder(sidecarMutationRecorder{kind: "issue"}))
		}
	default:
		err = fmt.Errorf("publish-batch does not support repository provider %q", repo.Provider)
	}
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	ctx, cancel := providerCommandContext()
	defer cancel()
	runID, _, err := providerRunContext()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	batch, err := (decomposition.Publisher{
		Provider: provider,
		Leaser: decomposition.FileTargetLeaser{
			Directory: filepath.Join(layoutFor(root).SchedulerDir(), "decomposition-target-locks"),
		},
		Repo:  repo,
		RunID: runID,
	}).Publish(ctx, plan)
	if err != nil {
		return failProviderStage(stderr, "publish decomposition batch", err, "published-batch.json")
	}

	schedulerDir := layoutFor(root).SchedulerDir()
	instanceLog, _, err := journal.OpenInstanceLog(schedulerDir)
	if err != nil {
		pf(stderr, "error: open instance log: %v\n", err)
		return 1
	}
	defer func() { _ = instanceLog.Close() }()
	key := localscheduler.ClaimKey{
		Gaggle: providerGaggle(), Provider: string(repo.Provider), ExternalID: plan.Parent.ID,
	}
	if err := releaseSelectSourceParent(schedulerDir, instanceLog, key, runID); err != nil {
		pf(stderr, "error: release parent claim: %v\n", err)
		return 1
	}

	childIDs := make([]string, 0, len(batch.Children))
	for _, child := range batch.Children {
		childIDs = append(childIDs, child.ID)
	}
	data, err := json.Marshal(publishBatchResult{ParentID: plan.Parent.ID, PlanDigest: batch.PlanDigest, ChildIDs: childIDs})
	if err != nil {
		pf(stderr, "error: marshal published batch result: %v\n", err)
		return 1
	}
	resultFile := providerInput("resultFile", "published-batch.json")
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}
	pf(stdout, "published decomposition batch for parent %s with %d child(ren)\n", plan.Parent.ID, len(batch.Children))
	return 0
}
