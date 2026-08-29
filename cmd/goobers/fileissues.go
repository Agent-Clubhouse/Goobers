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
	"idempotency key. It never applies goobers:approved: that is the SEC-047\n" +
	"trust decision and a maintainer supplies it.\n\n" +
	"With --check, only validate the artifact and run the read-only dedupe\n" +
	"scan (github:issues:read); nothing is created. The write path must be\n" +
	"bound to a --check that marked this artifact valid: wire the check\n" +
	"stage's nominationsDigest output to the checkDigest input (inputsFrom),\n" +
	"or point checkFile at its result, or run on a self runner where the\n" +
	"checkStage's recorded result is in the run journal.\n\n" +
	"Inputs: nominationsFile (nominations.json), producerStage (triage),\n" +
	"backlogLabel (required), partitionLabel (required), maxPerRun (3),\n" +
	"dedupeWindowDays (21), nominatedLabel, checkDigest, checkFile\n" +
	"(nomination-check.json), checkStage (validate-nominations),\n" +
	"resultFile (filed-nominations.json).\n" +
	"Exit codes: 0 = filed or checked / 1 = business or provider error / 2 = usage error.\n"

const (
	fileIssuesResultFileName = "filed-nominations.json"
	fileIssuesCheckFileName  = "nomination-check.json"
	fileIssuesArtifactName   = "nominations.json"
)

// fileIssuesCheckResult is `file-issues --check`'s result file; its keys are
// the stage outputs an automated gate routes on. NominationsDigest is set
// only when Valid, so a write stage bound through the checkDigest input
// (inputsFrom: nominationsDigest) is bound to a check that marked the
// artifact valid — an invalid check carries no digest to bind to.
type fileIssuesCheckResult struct {
	Valid             bool                     `json:"valid"`
	NominationsDigest string                   `json:"nominationsDigest"`
	FiledCount        int                      `json:"filedCount"`
	SuppressedCount   int                      `json:"suppressedCount"`
	OverBudget        int                      `json:"overBudget"`
	Errors            []string                 `json:"errors"`
	SchemaInvalid     bool                     `json:"schemaInvalid,omitempty"`
	Suppressions      []nomination.Suppression `json:"suppressions,omitempty"`
	Overflow          []string                 `json:"overflow,omitempty"`
}

// fileIssuesResult is `file-issues`' result file.
type fileIssuesResult struct {
	NominationsDigest string                   `json:"nominationsDigest"`
	Created           int                      `json:"created"`
	Filed             int                      `json:"filed"`
	Suppressed        int                      `json:"suppressed"`
	Overflow          int                      `json:"overflow"`
	Refused           int                      `json:"refused"`
	Issues            []nomination.FiledIssue  `json:"issues"`
	Suppressions      []nomination.Suppression `json:"suppressions,omitempty"`
	OverflowKeys      []string                 `json:"overflowKeys,omitempty"`
	Refusals          []nomination.Refusal     `json:"refusals,omitempty"`
	Annotated         []string                 `json:"annotated,omitempty"`
}

func runFileIssues(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("file-issues", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "file-issues")
	checkOnly := fs.Bool("check", false, "validate the artifact and run the read-only dedupe scan without creating issues")
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
	if !validation.Valid {
		if *checkOnly {
			return writeFileIssuesCheck(stdout, stderr, resultFile, fileIssuesCheckResult{
				Errors: validation.Errors, SchemaInvalid: validation.SchemaInvalid,
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

	publisher := nomination.Publisher{Repo: repo, RunID: runID, Policy: policy}
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
	}

	ctx, cancel := providerCommandContext()
	defer cancel()
	if *checkOnly {
		plan, err := publisher.Scan(ctx, artifact)
		if err != nil {
			return failProviderStage(stderr, "scan nominations", err, fileIssuesResultFileName)
		}
		return writeFileIssuesCheck(stdout, stderr, resultFile, fileIssuesCheckResult{
			Valid: true, NominationsDigest: plan.Digest, FiledCount: len(plan.File),
			SuppressedCount: len(plan.Suppressed), OverBudget: len(plan.Overflow),
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
	data, err := json.Marshal(fileIssuesResult{
		NominationsDigest: result.Digest,
		Created:           created,
		Filed:             len(result.Filed),
		Suppressed:        len(result.Suppressed),
		Overflow:          len(result.Overflow),
		Refused:           len(result.Refused),
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
	pf(stdout, "filed %d nomination(s) (%d created), suppressed %d, %d over budget, %d refused\n",
		len(result.Filed), created, len(result.Suppressed), len(result.Overflow), len(result.Refused))
	return 0
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
