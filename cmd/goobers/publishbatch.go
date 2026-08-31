package main

import (
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/decomposition"
	"github.com/goobers/goobers/internal/journal"
)

const publishBatchHelp = "Usage: goobers publish-batch [path]\n\n" +
	"publish-batch is the decomposition workflow's deterministic publication\n" +
	"stage (docs/design/decomposition-workflow.md §5.2-5.3, DEC-4). It consumes\n" +
	"a validated decomposition plan, resumes the prepare/publish protocol by\n" +
	"stable markers, verifies the complete batch, commits eligibility through\n" +
	"the parent published record, and releases the parent claim.\n\n" +
	"Exit codes: 0 = batch published / 1 = business or provider error / 2 = usage error.\n"

type publishBatchResult struct {
	ParentID            string   `json:"parentId"`
	PlanDigest          string   `json:"planDigest"`
	ChildIDs            []string `json:"childIds"`
	PublicationConflict bool     `json:"publicationConflict"`
	ConflictReason      string   `json:"conflictReason,omitempty"`
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
	plan, err := readDecompositionInput[decomposition.Plan](root, providerInput("planFile", "plan.json"), "plan.json", "design-slices", "/plan.json")
	if err != nil {
		pf(stderr, "error: read plan: %v\n", err)
		return 1
	}
	validation, err := readDecompositionInput[validatePlanResult](root, providerInput("validationFile", "plan-validation.json"), "plan-validation.json", "validate-plan", "/result")
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
	// The batch is published into the BACKLOG container select-source claimed
	// the parent in — parent update, child creation, hierarchy links, and every
	// marker comment. Fail-loud resolution, like select-source's: publishing a
	// batch into the wrong container is not recoverable by retry.
	backlogRepo, err := backlogRepositoryRefForStage(root, repo)
	if err != nil {
		pf(stderr, "error: resolve backlog repository: %v\n", err)
		return 1
	}
	// One constructor for all three providers: built for the backlog repository
	// and authenticated as spec.backlog.connectionRef, so a cross-account
	// backlog is never reached with the project's capability token. The
	// per-provider switch this replaced hard-coded the routed repo and the
	// project credential for GitHub/Gitea and the routed repo for ADO.
	backlogProvider, err := newBacklogProviderForStage(root, repo, backlogRepo, false,
		withStageProviderCache(), withStageProviderMutations("issue"))
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	provider, ok := backlogProvider.(publishBatchProvider)
	if !ok {
		pf(stderr, "error: publish-batch does not support repository provider %q\n", backlogRepo.Provider)
		return 1
	}

	ctx, cancel := providerCommandContext()
	defer cancel()
	runID, _, err := providerRunContext()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	gaggle := providerGaggle()
	// Resolved before publication so the release below can never fall back to a
	// gaggle-scoped key that would leave the authoritative backlog-scoped lease
	// select-source took held until it expires.
	backlogIdentity, identityErr := backlogIdentityForStage(root, repo)
	if identityErr != nil && gaggle != "" {
		pf(stderr, "error: resolve backlog identity: %v\n", identityErr)
		return 1
	}
	batch, err := (decomposition.Publisher{
		Provider: provider,
		Leaser: decomposition.FileTargetLeaser{
			Directory: filepath.Join(layoutFor(root).SchedulerDir(), "decomposition-target-locks"),
		},
		Repo:  backlogRepo,
		RunID: runID,
	}).Publish(ctx, plan)
	if err != nil {
		if decomposition.IsPublicationConflict(err) {
			resultFile := providerInput("resultFile", "published-batch.json")
			data, marshalErr := json.Marshal(publishBatchResult{
				ParentID:            plan.Parent.ID,
				PlanDigest:          planDigest,
				PublicationConflict: true,
				ConflictReason:      err.Error(),
			})
			if marshalErr != nil {
				pf(stderr, "error: marshal publication conflict result: %v\n", marshalErr)
				return 1
			}
			if writeErr := os.WriteFile(resultFile, data, 0o644); writeErr != nil {
				pf(stderr, "error: write %s: %v\n", resultFile, writeErr)
				return 1
			}
			pf(stdout, "decomposition batch for parent %s has a publication conflict: %s\n", plan.Parent.ID, err)
			return 0
		}
		return failProviderStage(stderr, "publish decomposition batch", err, "published-batch.json")
	}

	schedulerDir := layoutFor(root).SchedulerDir()
	instanceLog, _, err := journal.OpenInstanceLog(schedulerDir)
	if err != nil {
		pf(stderr, "error: open instance log: %v\n", err)
		return 1
	}
	defer func() { _ = instanceLog.Close() }()
	// The exact key select-source claimed under, rebuilt from the same
	// resolution inputs — a mismatched key silently no-ops the release
	// (ClaimLedger.release is idempotent by design) and strands the parent's
	// lease until it expires.
	key := selectSourceClaimKey(backlogIdentity, gaggle, backlogRepo, plan.Parent.ID)
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
