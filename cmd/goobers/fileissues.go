package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/nomination"
	"github.com/goobers/goobers/providers"
)

const fileIssuesHelp = "Usage: goobers file-issues [--check] [--auto-approve] [path]\n\n" +
	"file-issues is the nomination workflows' deterministic issue filer —\n" +
	"TBH-1 #2251's first slice, built on the decomposition binding (a typed\n" +
	"goobers.dev/nominations/v1 artifact, a digest-bound check, a publisher\n" +
	"that owns every goobers:* label). The finder proposes area/type labels\n" +
	"and evidence; this stage dedupes by body marker against every issue\n" +
	"carrying the nominated label, excludes anything flake-watch already\n" +
	"fingerprints, enforces maxPerRun, and creates issues with a retry-safe\n" +
	"idempotency key.\n\n" +
	"With --check, only validate the artifact and run the read-only dedupe\n" +
	"scan (github:issues:read); nothing is created. --auto-approve (or the\n" +
	"autoApprove=low-risk-only input) applies goobers:approved with the\n" +
	"github:issues:approve credential, and only to low-risk nominations that\n" +
	"clear every precondition; the default never approves.\n\n" +
	"Inputs: nominationsFile (nominations.json), partitionLabel (required),\n" +
	"maxPerRun (3), dedupeWindowDays (21), backlogLabel (goobers),\n" +
	"nominatedLabel, autoApprove (never), resultFile (filed-nominations.json).\n" +
	"Exit codes: 0 = filed or checked / 1 = business or provider error / 2 = usage error.\n"

const (
	fileIssuesResultFileName = "filed-nominations.json"
	fileIssuesCheckFileName  = "nomination-check.json"
	fileIssuesArtifactName   = "nominations.json"
)

// fileIssuesCheckResult is `file-issues --check`'s result file; its keys are
// the stage outputs an automated gate routes on.
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
	Approved          int                      `json:"approved"`
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
	autoApproveFlag := fs.Bool("auto-approve", false, "apply goobers:approved to low-risk nominations that clear every precondition")
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
	validation := nomination.Validate(artifact)
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

	policy, err := fileIssuesPolicy(*autoApproveFlag)
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
		if policy.AutoApprove {
			// The approve label is applied by the capability's own credential
			// — never the write token — so the far-side label-event ledger
			// names the approving identity. A missing credential is a
			// precondition failure, not a crash: everything files unapproved.
			approveToken, err := providerToken(capability.GitHubIssuesApprove)
			if err != nil {
				pf(stderr, "warning: auto-approve requested but %v; filing every nomination unapproved\n", err)
			} else {
				publisher.Approver = newCachedGitHubProvider(root, approveToken, providers.WithMutationRecorder(sidecarMutationRecorder{kind: "issue"}))
			}
		}
		publisher.Verifier = newStageEvidenceVerifier(root)
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
		Approved:          result.Approved(),
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
	pf(stdout, "filed %d nomination(s) (%d created, %d approved), suppressed %d, %d over budget, %d refused\n",
		len(result.Filed), created, result.Approved(), len(result.Suppressed), len(result.Overflow), len(result.Refused))
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

// fileIssuesPolicy reads the publisher policy from stage inputs. The
// partition label has no default: it is the instance's claim partition and
// an issue filed without it is invisible to this instance's own curation.
func fileIssuesPolicy(autoApproveFlag bool) (nomination.Policy, error) {
	policy := nomination.Policy{
		BacklogLabel:   providerInput("backlogLabel", "goobers"),
		PartitionLabel: providerInput("partitionLabel", ""),
		NominatedLabel: providerInput("nominatedLabel", providers.LabelNominated),
		AutoApprove:    autoApproveFlag,
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
	if !policy.AutoApprove {
		switch mode := strings.ToLower(strings.TrimSpace(providerInput("autoApprove", "never"))); mode {
		case "", "never", "false":
		case "low-risk-only", "true":
			policy.AutoApprove = true
		default:
			return nomination.Policy{}, fmt.Errorf("autoApprove input must be never or low-risk-only, got %q", mode)
		}
	}
	return policy, nil
}

// bindFileIssuesCheck mirrors publish-batch's plan binding: when a
// `file-issues --check` result is reachable, the write must be over the
// artifact it marked valid. Absence is allowed (standalone use) because the
// write path validates the artifact itself.
func bindFileIssuesCheck(root, digest string) error {
	check, err := readDecompositionInput[fileIssuesCheckResult](root, providerInput("checkFile", fileIssuesCheckFileName), fileIssuesCheckFileName, providerInput("checkStage", "validate-nominations"), "/result")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
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

// stageEvidenceVerifier checks evidence against what this process can
// observe: run journals under the instance root (a self runner) and stage
// artifacts materialized into the working directory (any runner). On a
// stage pod the journal is unreachable, so only artifact digests verify.
type stageEvidenceVerifier struct {
	layout    instance.Layout
	workspace string
	journals  map[string]map[uint64]bool
}

func newStageEvidenceVerifier(root string) *stageEvidenceVerifier {
	workspace, err := os.Getwd()
	if err != nil {
		workspace = "."
	}
	return &stageEvidenceVerifier{layout: layoutFor(root), workspace: workspace, journals: map[string]map[uint64]bool{}}
}

func (v *stageEvidenceVerifier) VerifyJournal(runID string, seq uint64) bool {
	if !apiv1.ValidRunID(runID) || seq == 0 {
		return false
	}
	seqs, cached := v.journals[runID]
	if !cached {
		seqs = map[uint64]bool{}
		v.journals[runID] = seqs
		dir, err := runDirFor(v.layout, runID)
		if err != nil {
			return false
		}
		reader, err := journal.OpenReadOnly(dir)
		if err != nil {
			return false
		}
		events, err := reader.Events()
		if err != nil {
			return false
		}
		for _, event := range events {
			seqs[event.Seq] = true
		}
	}
	return seqs[seq]
}

func (v *stageEvidenceVerifier) VerifyArtifact(path, digest string) bool {
	full, err := apiv1.ResolveContainedPath(v.workspace, path)
	if err != nil {
		return false
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return false
	}
	sum := sha256.Sum256(data)
	return "sha256:"+hex.EncodeToString(sum[:]) == digest
}
