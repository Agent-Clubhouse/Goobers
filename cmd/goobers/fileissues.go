package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/internal/nomination"
	"github.com/goobers/goobers/providers"
)

const fileIssuesHelp = "Usage: goobers file-issues [--check] [path]\n\n" +
	"file-issues is the nomination workflows' deterministic issue filer —\n" +
	"TBH-1 #2251's first slice, built on the decomposition binding (a typed\n" +
	"goobers.dev/nominations/v1 artifact, a digest-bound check, a publisher\n" +
	"that owns every goobers:* label). The finder proposes area/type labels\n" +
	"and evidence; this stage dedupes by body marker against every issue\n" +
	"carrying the nominated label, excludes anything flake-watch already\n" +
	"fingerprints, enforces maxPerRun, and creates issues with a retry-safe\n" +
	"idempotency key.\n\n" +
	"goobers:approved (the SEC-047 trust label) is applied on one condition\n" +
	"only (decision 004): the nomination's evidence names a finding — a go\n" +
	"vet diagnostic, a golangci-lint issue (linter + file + line) or a go\n" +
	"test failure (package + test) — that the deterministic signalsStage's\n" +
	"stdout artifact of THIS run contains byte for byte, as parsed by this\n" +
	"stage from the run journal; plus riskClass low, type:bug, one package,\n" +
	"no load-bearing path, no needs-human trigger, and no open or recently\n" +
	"closed nominated issue naming the same finding (a body marker the\n" +
	"filer computes from the finding, not the model's dedupeKey), nor an\n" +
	"earlier nomination of the same artifact naming it. autoApprove=\n" +
	"deterministic-only (exactly; default never) opts in and the label is\n" +
	"added with the github:issues:approve credential only.\n" +
	"Everything else files unapproved with the reasons in the result. The\n" +
	"journal read goes through the run-scoped journal plane, so a dispatched\n" +
	"stage pod confirms against the same artifact a daemon host does; a\n" +
	"half-configured plane is a hard failure, and a run whose signals stage\n" +
	"recorded nothing simply approves nothing and says so.\n\n" +
	"With --check, only validate the artifact and run the read-only dedupe\n" +
	"scan (github:issues:read); nothing is created. The write path must be\n" +
	"bound to a --check that marked this artifact valid: wire the check\n" +
	"stage's nominationsDigest output to the checkDigest input (inputsFrom),\n" +
	"or point checkFile at its result, or run on a self runner where the\n" +
	"checkStage's recorded result is in the run journal.\n\n" +
	"Inputs: nominationsFile (nominations.json), producerStage (triage),\n" +
	"backlogLabel (required), partitionLabel (required), maxPerRun (3),\n" +
	"dedupeWindowDays (21), nominatedLabel, autoApprove (never), signalsStage\n" +
	"(collect-repo-signals), checkDigest, checkFile (nomination-check.json),\n" +
	"checkStage (validate-nominations), resultFile (filed-nominations.json).\n" +
	"Exit codes: 0 = filed or checked / 1 = business or provider error / 2 = usage error.\n"

const (
	fileIssuesResultFileName = "filed-nominations.json"
	fileIssuesCheckFileName  = "nomination-check.json"
	fileIssuesArtifactName   = "nominations.json"
	// fileIssuesSignalsStage is the default deterministic stage whose stdout
	// artifact carries the raw go vet / golangci-lint / go test output a
	// nomination's finding evidence is confirmed against (the signalsStage
	// input).
	fileIssuesSignalsStage = "collect-repo-signals"
)

// fileIssuesCheckResult is `file-issues --check`'s result file; its keys are
// the stage outputs an automated gate routes on. NominationsDigest is set
// only when Valid, so a write stage bound through the checkDigest input
// (inputsFrom: nominationsDigest) is bound to a check that marked the
// artifact valid — an invalid check carries no digest to bind to.
type fileIssuesCheckResult struct {
	Valid             bool   `json:"valid"`
	NominationsDigest string `json:"nominationsDigest"`
	FiledCount        int    `json:"filedCount"`
	SuppressedCount   int    `json:"suppressedCount"`
	OverBudget        int    `json:"overBudget"`
	// ApprovableCount is how many of the candidates to file clear every
	// deterministic approval bound — what a write with
	// autoApprove=deterministic-only and the approve credential would
	// approve.
	ApprovableCount int                      `json:"approvableCount"`
	Findings        fileIssuesFindings       `json:"findings"`
	Errors          []string                 `json:"errors"`
	SchemaInvalid   bool                     `json:"schemaInvalid,omitempty"`
	Suppressions    []nomination.Suppression `json:"suppressions,omitempty"`
	Overflow        []string                 `json:"overflow,omitempty"`
}

// fileIssuesResult is `file-issues`' result file.
type fileIssuesResult struct {
	NominationsDigest string                   `json:"nominationsDigest"`
	Created           int                      `json:"created"`
	Filed             int                      `json:"filed"`
	Approved          int                      `json:"approved"`
	Unapproved        int                      `json:"unapproved"`
	Suppressed        int                      `json:"suppressed"`
	Overflow          int                      `json:"overflow"`
	Refused           int                      `json:"refused"`
	Findings          fileIssuesFindings       `json:"findings"`
	Issues            []nomination.FiledIssue  `json:"issues"`
	Suppressions      []nomination.Suppression `json:"suppressions,omitempty"`
	OverflowKeys      []string                 `json:"overflowKeys,omitempty"`
	Refusals          []nomination.Refusal     `json:"refusals,omitempty"`
	Annotated         []string                 `json:"annotated,omitempty"`
}

// fileIssuesFindings summarizes the deterministic tool findings the stage
// could confirm nominations against: where they came from, or why none could
// be read.
type fileIssuesFindings struct {
	Available bool     `json:"available"`
	Stage     string   `json:"stage"`
	Reason    string   `json:"reason,omitempty"`
	Vet       int      `json:"vet"`
	Lint      int      `json:"lint"`
	Test      int      `json:"test"`
	Problems  []string `json:"problems,omitempty"`
}

func runFileIssues(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("file-issues", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "file-issues")
	checkOnly := fs.Bool("check", false, "validate the artifact and run the read-only dedupe scan without creating issues")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root, ok := providerStageRootArg(fs)
	if !ok {
		return 2
	}
	resultFile := providerInput("resultFile", fileIssuesResultFileName)

	runID, _, err := providerRunContext()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	producerStage := providerInput("producerStage", "triage")
	artifact, err := readDecompositionInput[nomination.Artifact](root, providerInput("nominationsFile", fileIssuesArtifactName), fileIssuesArtifactName, producerStage, "/"+fileIssuesArtifactName)
	if err != nil {
		pf(stderr, "error: read nominations artifact: %v\n", err)
		return 1
	}
	validation := nomination.Validate(artifact, runID)
	digest := ""
	if validation.Valid {
		if digest, err = nomination.Digest(artifact); err != nil {
			pf(stderr, "error: digest nominations artifact: %v\n", err)
			return 1
		}
	}
	signalsStage := providerInput("signalsStage", fileIssuesSignalsStage)
	if !validation.Valid {
		if *checkOnly {
			return writeFileIssuesCheck(stdout, stderr, resultFile, fileIssuesCheckResult{
				Errors: validation.Errors, SchemaInvalid: validation.SchemaInvalid, Findings: fileIssuesFindings{Stage: signalsStage},
			})
		}
		pf(stderr, "error: refusing to file an invalid nominations artifact: %s\n", strings.Join(validation.Errors, "; "))
		return 1
	}

	policy, err := fileIssuesPolicy()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	// The tool findings a nomination can be confirmed against come from the
	// deterministic signals stage's stdout artifact of this run, read from
	// the run journal — never from a file the finder could have written. On
	// a stage pod that read goes over the run-scoped journal plane
	// (Goobers#3996 blocker 2), so a pod confirms exactly what the daemon
	// would. An artifact that genuinely is not there means nothing matches
	// and nothing is approved; the reason is recorded, not hidden. A plane
	// that is configured but unusable is fatal instead — see
	// loadFileIssuesFindings.
	findings, findingsSummary, err := loadFileIssuesFindings(root, runID, signalsStage)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if !findingsSummary.Available {
		pf(stderr, "warning: %s; no nomination can be approved\n", findingsSummary.Reason)
	}
	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if repo.Provider != providers.ProviderGitHub {
		pf(stderr, "error: file-issues does not support repository provider %q\n", repo.Provider)
		return 1
	}
	if !*checkOnly {
		if err := bindFileIssuesCheck(root, digest); err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
	}

	publisher := nomination.Publisher{Repo: repo, RunID: runID, Policy: policy, Findings: findings, FindingsUnavailable: findingsSummary.Reason}
	if *checkOnly {
		token, err := providerToken(capability.GitHubIssuesRead)
		if err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
		publisher.Provider = newCachedGitHubProvider(root, token)
	} else {
		token, err := providerToken(capability.GitHubIssuesWrite)
		if err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
		publisher.Provider = newCachedGitHubProvider(root, token, providers.WithMutationRecorder(sidecarMutationRecorder{kind: "issue"}))
		if policy.AutoApprove {
			// The approve label is applied by the capability's own credential
			// — never the write token — so the far-side label-event ledger
			// names the approving identity. A missing credential is an unmet
			// bound, not a crash: everything files unapproved and says so.
			approveToken, err := providerToken(capability.GitHubIssuesApprove)
			if err != nil {
				pf(stderr, "warning: autoApprove is deterministic-only but %v; filing every nomination unapproved\n", err)
			} else {
				publisher.Approver = newCachedGitHubProvider(root, approveToken, providers.WithMutationRecorder(sidecarMutationRecorder{kind: "issue"}))
			}
		}
	}

	ctx, cancel := providerCommandContext()
	defer cancel()
	if *checkOnly {
		plan, err := publisher.Scan(ctx, artifact)
		if err != nil {
			return failProviderStage(stderr, "scan nominations", err, fileIssuesResultFileName)
		}
		approvable := 0
		for _, cand := range plan.File {
			if len(cand.ApprovalUnmet) == 0 {
				approvable++
			}
		}
		return writeFileIssuesCheck(stdout, stderr, resultFile, fileIssuesCheckResult{
			Valid: true, NominationsDigest: plan.Digest, FiledCount: len(plan.File),
			SuppressedCount: len(plan.Suppressed), OverBudget: len(plan.Overflow),
			ApprovableCount: approvable, Findings: findingsSummary,
			Errors: []string{}, Suppressions: plan.Suppressed, Overflow: plan.Overflow,
		})
	}

	result, err := publisher.Publish(ctx, artifact)
	if err != nil {
		return failProviderStage(stderr, "file nominations", err, fileIssuesResultFileName)
	}
	created := 0
	for _, issue := range result.Filed {
		if !issue.Reused {
			created++
		}
	}
	approved := result.Approved()
	data, err := json.Marshal(fileIssuesResult{
		NominationsDigest: result.Digest,
		Created:           created,
		Filed:             len(result.Filed),
		Approved:          approved,
		Unapproved:        len(result.Filed) - approved,
		Suppressed:        len(result.Suppressed),
		Overflow:          len(result.Overflow),
		Refused:           len(result.Refused),
		Findings:          findingsSummary,
		Issues:            nonNilFiled(result.Filed),
		Suppressions:      result.Suppressed,
		OverflowKeys:      result.Overflow,
		Refusals:          result.Refused,
		Annotated:         result.Annotated,
	})
	if err != nil {
		pf(stderr, "error: marshal filed-nominations result: %v\n", err)
		return 1
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}
	pf(stdout, "filed %d nomination(s) (%d created, %d approved, %d unapproved), suppressed %d, %d over budget, %d refused\n",
		len(result.Filed), created, approved, len(result.Filed)-approved, len(result.Suppressed), len(result.Overflow), len(result.Refused))
	for _, issue := range result.Filed {
		if !issue.Approved {
			pf(stdout, "unapproved %q (#%s): %s\n", issue.Key, issue.IssueID, strings.Join(issue.ApprovalUnmet, "; "))
		}
	}
	return 0
}

// loadFileIssuesFindings reads the signals stage's stdout artifact of this
// run through the run journal — the same seam readDecompositionInput's
// journal arm uses — and parses the tool findings out of it. There is
// deliberately no file arm: a file in the working directory could have been
// written by anything, while the journal artifact is what the runner
// recorded from the deterministic stage's own stdout.
//
// Goobers#3996 blocker 2 gave that read a POD ROUTE. It used to open a run
// directory under the instance root by path, which in a pod is an unstamped
// GOOBERS_INSTANCE_ROOT falling back to "." — so the read always failed there
// and the lane filed every nomination unapproved while exiting 0. It now goes
// through stageRunJournal like every other converted reader, so on the plane
// the identical question is answered by the daemon for this run and only this
// run.
//
// A read that cannot be served is still not fatal — decision 004's bar is
// "approve only what can be confirmed", so an unconfirmable nomination files
// UNAPPROVED with the reason rather than failing the stage. What IS fatal is
// a half-wired plane (fileIssuesFindingsFatal): an endpoint with no bearer or
// no run identity means this stage cannot tell "there is no finding" from "I
// was not allowed to look", and quietly choosing the first is the
// silent-wrong-result class the seam exists to close.
func loadFileIssuesFindings(root, runID, stage string) (*nomination.Findings, fileIssuesFindings, error) {
	summary := fileIssuesFindings{Stage: stage}
	data, err := readStageStdoutArtifact(root, runID, stage)
	if err != nil {
		if fileIssuesFindingsFatal(err) {
			return nil, summary, fmt.Errorf(
				"the journal plane is configured but unusable, so the %s stdout artifact of run %s cannot be read: %w",
				stage, runID, err)
		}
		summary.Reason = fmt.Sprintf(
			"the %s stdout artifact of run %s is not readable from this stage (%v), so no nomination can be confirmed against a tool finding",
			stage, runID, err)
		return nil, summary, nil
	}
	findings := nomination.ParseSignals(data)
	summary.Available = true
	summary.Vet = findings.Counts[nomination.ToolVet]
	summary.Lint = findings.Counts[nomination.ToolLint]
	summary.Test = findings.Counts[nomination.ToolTest]
	summary.Problems = findings.Problems
	return findings, summary, nil
}

// fileIssuesFindingsFatal reports the failures that must stop the stage
// instead of degrading it to "nothing can be approved": a journal plane that
// is configured but incomplete, and a plane that is configured for a
// different run than this stage claims to be. Both are misconfiguration, not
// absence, and both would otherwise be indistinguishable from a run whose
// signals stage simply produced nothing.
func fileIssuesFindingsFatal(err error) bool {
	return errors.Is(err, journalclient.ErrEndpointWithoutToken) ||
		errors.Is(err, journalclient.ErrEndpointWithoutRun) ||
		errors.Is(err, ErrPlaneRunMismatch)
}

// fileIssuesStdoutArtifactSuffix is the name the executor records every
// deterministic stage's captured stdout under ("<task>/stdout.log",
// internal/executor/shell.go), listed in that stage's stage.finished event.
const fileIssuesStdoutArtifactSuffix = "/stdout.log"

// maxFileIssuesSignalsBytes bounds the signals artifact this stage will parse.
// The signals stage prints tool output, not blobs; a stdout capture larger
// than this is not something to confirm evidence against, and buffering it in
// a pod is how a stage container is OOM-killed instead of failing loudly.
// The executor's own stdout capture is truncated well below it.
const maxFileIssuesSignalsBytes = 8 << 20

// readStageStdoutArtifact returns the stdout artifact the run journal records
// for stage's newest successful finish, through whichever backend this stage
// is on. The reference is resolved from this run's OWN journal — no path, no
// digest and no run is taken from anywhere else — and the bytes are bounded,
// media-checked and digest-verified before they are returned
// (journalclient.StageArtifactContent).
func readStageStdoutArtifact(root, runID, stage string) ([]byte, error) {
	if runID == "" {
		return nil, errors.New("no run id")
	}
	reader, err := stageRunJournal(root, runID)
	if err != nil {
		return nil, err
	}
	content, err := journalclient.StageArtifactContent(reader, stage, fileIssuesStdoutArtifactSuffix, journalclient.ArtifactBounds{
		MaxBytes: maxFileIssuesSignalsBytes,
		// The executor declares stdout captures as text/plain. A generic or
		// absent declaration is admitted (the daemon's projection cannot
		// report a more specific one); a positively different type is not.
		MediaTypes: []string{"text/plain"},
	})
	if err != nil {
		if errors.Is(err, journalclient.ErrArtifactNotRecorded) {
			return nil, fmt.Errorf("the run journal records no successful %s stage with a stdout artifact", stage)
		}
		return nil, err
	}
	return content.Bytes, nil
}

func nonNilFiled(filed []nomination.FiledIssue) []nomination.FiledIssue {
	if filed == nil {
		return []nomination.FiledIssue{}
	}
	return filed
}

func writeFileIssuesCheck(stdout, stderr io.Writer, resultFile string, result fileIssuesCheckResult) int {
	if result.Errors == nil {
		result.Errors = []string{}
	}
	data, err := json.Marshal(result)
	if err != nil {
		pf(stderr, "error: marshal nomination check result: %v\n", err)
		return 1
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}
	if result.Valid {
		pf(stdout, "nominations are valid: %d to file, %d suppressed, %d over budget\n", result.FiledCount, result.SuppressedCount, result.OverBudget)
	} else {
		pf(stdout, "nominations are invalid (%d finding(s))\n", len(result.Errors))
	}
	return 0
}

// fileIssuesPolicy reads the publisher policy from stage inputs. The backlog
// and partition labels have no defaults: they are the instance's backlog
// label and claim partition, and an issue filed without either is invisible
// to this instance's own curation — the same reason backlog-assignment takes
// its trust label as an explicit input rather than a literal.
func fileIssuesPolicy() (nomination.Policy, error) {
	policy := nomination.Policy{
		BacklogLabel:   strings.TrimSpace(providerInput("backlogLabel", "")),
		PartitionLabel: strings.TrimSpace(providerInput("partitionLabel", "")),
		NominatedLabel: providerInput("nominatedLabel", providers.LabelNominated),
	}
	if policy.BacklogLabel == "" {
		return nomination.Policy{}, errors.New("backlogLabel input is required (the instance's backlog label, the one backlog-query's requireLabels demands)")
	}
	if policy.PartitionLabel == "" {
		return nomination.Policy{}, errors.New("partitionLabel input is required (the instance's claim partition label, e.g. the label backlog-query's requireLabels demands)")
	}
	maxPerRun, err := strconv.Atoi(providerInput("maxPerRun", "3"))
	if err != nil || maxPerRun <= 0 {
		return nomination.Policy{}, fmt.Errorf("maxPerRun input must be a positive integer, got %q", providerInput("maxPerRun", "3"))
	}
	policy.MaxPerRun = maxPerRun
	days, err := strconv.Atoi(providerInput("dedupeWindowDays", "21"))
	if err != nil || days < 0 {
		return nomination.Policy{}, fmt.Errorf("dedupeWindowDays input must be a non-negative integer, got %q", providerInput("dedupeWindowDays", "21"))
	}
	policy.DedupeWindow = time.Duration(days) * 24 * time.Hour
	// The vocabulary is exact, not case-folded: the workflow policy table
	// (internal/workflow/*/policy_actions.go) prescribes approve-issue for
	// the literal "deterministic-only" only, so any other spelling must be
	// refused here rather than accepted as the opt-in — otherwise a lane
	// could approve without having declared approve-issue at admission.
	switch mode := strings.TrimSpace(providerInput("autoApprove", "never")); mode {
	case "", "never":
	case "deterministic-only":
		policy.AutoApprove = true
	default:
		return nomination.Policy{}, fmt.Errorf("autoApprove input must be never or deterministic-only (exactly), got %q", mode)
	}
	return policy, nil
}

// bindFileIssuesCheck binds the write to a `file-issues --check` that marked
// this exact artifact valid, and fails closed when no check is reachable —
// the same rule publish-batch applies to its plan validation. The binding is
// read from, in order: the checkDigest input (the check stage's
// nominationsDigest output carried by inputsFrom — the runner-agnostic
// shape, the only one a stage pod can use); the checkFile result on disk;
// the checkStage's result recorded in the run journal (a self runner).
func bindFileIssuesCheck(root, digest string) error {
	if bound, present := os.LookupEnv(executor.InputEnvVar("checkDigest")); present {
		switch strings.TrimSpace(bound) {
		case "":
			return errors.New("refusing to file nominations that file-issues --check did not mark valid (checkDigest input is empty)")
		case digest:
			return nil
		default:
			return errors.New("nominations do not match the artifact file-issues --check marked valid (checkDigest input)")
		}
	}
	checkStage := providerInput("checkStage", "validate-nominations")
	check, err := readDecompositionInput[fileIssuesCheckResult](root, providerInput("checkFile", fileIssuesCheckFileName), fileIssuesCheckFileName, checkStage, "/result")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no file-issues --check result to bind to (%w): wire the %s stage's nominationsDigest output to the checkDigest input, set checkFile, or run where the run journal records the %s result", err, checkStage, checkStage)
		}
		return fmt.Errorf("read nomination check: %w", err)
	}
	if !check.Valid {
		return errors.New("refusing to file nominations that file-issues --check did not mark valid")
	}
	if check.NominationsDigest != digest {
		return errors.New("nominations do not match the artifact file-issues --check marked valid")
	}
	return nil
}
