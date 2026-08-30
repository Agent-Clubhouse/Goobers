package nomination

import (
	"context"
	"fmt"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/flake"
	"github.com/goobers/goobers/providers"
)

// Provider is the issue surface the publisher writes through with the
// github:issues:write credential.
type Provider interface {
	ListWorkItems(context.Context, providers.ListWorkItemsRequest) ([]providers.WorkItem, error)
	GetWorkItem(context.Context, providers.RepositoryRef, string) (providers.WorkItem, error)
	CreateWorkItem(context.Context, providers.CreateWorkItemRequest) (providers.WorkItem, error)
	UpdateWorkItem(context.Context, providers.UpdateWorkItemRequest) (providers.WorkItem, error)
	ListComments(context.Context, providers.RepositoryRef, string) ([]providers.Comment, error)
	CreateWorkItemComment(context.Context, providers.RepositoryRef, string, string) (providers.Comment, error)
}

// Approver is the surface that applies goobers:approved. It is a separate
// value from Provider because it must be authenticated by the
// github:issues:approve credential — never the write credential — so the
// label-event ledger on the far side names the approving identity. It is
// used for the label add and nothing else.
type Approver interface {
	UpdateWorkItem(context.Context, providers.UpdateWorkItemRequest) (providers.WorkItem, error)
}

// Policy is the publisher's deterministic label and budget policy, sourced
// from stage inputs — never from the artifact.
//
// goobers:approved is the SEC-047 trust label (docs/requirements/security.md).
// The publisher applies it on one condition only (engagement decision 004):
// the nomination names a finding — a go vet diagnostic, a golangci-lint
// issue, or a go test failure — that the deterministic collect-repo-signals
// stage's own tool output contains byte for byte (Publisher.Findings), and
// clears the mechanical bounds in approvalUnmet. Every other precondition a
// filer could check is written by the finder's own model; a tool the model
// does not control reporting the defect is the one thing it cannot forge.
// Anything whose only evidence is a telemetry aggregate, a journal pointer
// or a source location the model chose files goobers:nominated, unapproved,
// and waits for a maintainer.
type Policy struct {
	// BacklogLabel (the instance's backlog label) and PartitionLabel (its
	// claim partition, e.g. goobers:cloud) are applied to every filed issue so
	// it is visible to this instance's own curation.
	BacklogLabel   string
	PartitionLabel string
	// NominatedLabel marks every filed issue and keys the dedupe scan.
	NominatedLabel string
	// MaxPerRun caps creates per run; the remainder is reported as overflow.
	MaxPerRun int
	// DedupeWindow applies only to CLOSED matches: an open match always
	// suppresses, regardless of age.
	DedupeWindow time.Duration
	// AutoApprove opts the stage into applying goobers:approved to
	// nominations that match a deterministic tool finding and clear every
	// mechanical bound (the autoApprove: deterministic-only input). Default
	// off: everything files unapproved.
	AutoApprove bool
}

func (p Policy) validate() error {
	for _, field := range []struct{ name, value string }{
		{"backlog label", p.BacklogLabel}, {"partition label", p.PartitionLabel}, {"nominated label", p.NominatedLabel},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("publisher policy needs a %s", field.name)
		}
	}
	if p.MaxPerRun <= 0 {
		return fmt.Errorf("publisher policy maxPerRun must be positive, got %d", p.MaxPerRun)
	}
	if p.DedupeWindow < 0 {
		return fmt.Errorf("publisher policy dedupe window must not be negative")
	}
	return nil
}

// Publisher files nominations.
type Publisher struct {
	Provider Provider
	// Approver is nil when the github:issues:approve credential did not
	// resolve (or the stage did not opt in); then no nomination is approved
	// and every filed issue names that reason.
	Approver Approver
	// Findings is the parsed collect-repo-signals stdout artifact of THIS
	// run — the only source a nomination's finding evidence is confirmed
	// against. Nil when the artifact is unreachable (a stage pod cannot read
	// the run journal): then nothing matches, nothing is approved, and
	// FindingsUnavailable names why.
	Findings            *Findings
	FindingsUnavailable string
	Repo                providers.RepositoryRef
	// RunID is the stage's own run id (GOOBERS_RUN_ID). The artifact must
	// name the same run; it is what every marker, footer and provenance line
	// carries.
	RunID  string
	Policy Policy
	Now    func() time.Time
}

// Suppression records one nomination the dedupe scan kept out.
type Suppression struct {
	Key     string `json:"key"`
	Reason  string `json:"reason"`
	IssueID string `json:"issueId,omitempty"`
}

// Candidate is one nomination the scan admitted, in filing order.
type Candidate struct {
	Nomination Nomination
	KeyHash    string
	// Strength orders the budget: 2 when every finding pointer the
	// nomination carries is confirmed against the tool output (and there is
	// at least one), 1 otherwise. It is keyed on the deterministic match
	// alone, so the model cannot promote its own items by claiming evidence
	// kinds.
	Strength int
	// Findings are the confirmed tool findings, one per finding pointer, in
	// evidence order — empty unless every finding pointer matched.
	Findings []Finding
	// ApprovalUnmet names every deterministic approval bound the candidate
	// fails, evaluated by the scan. Empty means the write would approve it,
	// given the opt-in and the approve credential (file adds those two).
	ApprovalUnmet []string
	// OpenDuplicate is the open issue that suppressed this candidate, when
	// one did (such candidates are in Plan.Suppressed, not Plan.File).
	OpenDuplicate string
	// OwnedIssue is the issue this run already filed for the key (a retried
	// attempt found its filed marker in the dedupe listing), so filing reads
	// it back instead of creating — no reliance on the provider's search
	// index.
	OwnedIssue string
}

// Plan is the scan's read-only outcome.
type Plan struct {
	Digest     string
	File       []Candidate
	Overflow   []string
	Suppressed []Suppression
	// openDuplicates pairs suppressed keys with the open issue to annotate.
	openDuplicates map[string]Candidate
	// existingIDs are the issues that existed before this attempt filed
	// anything, so a create that returns one of them is a retry, not a
	// creation.
	existingIDs map[string]bool
}

// FiledIssue records one issue the publisher created or, on retry, found.
// Approved reports whether goobers:approved was applied (or already
// present) through the approve credential; ApprovalUnmet names every reason
// it was not.
type FiledIssue struct {
	Key           string   `json:"key"`
	IssueID       string   `json:"issueId"`
	URL           string   `json:"url,omitempty"`
	Labels        []string `json:"labels"`
	Approved      bool     `json:"approved"`
	ApprovalUnmet []string `json:"approvalUnmet,omitempty"`
	Reused        bool     `json:"reused,omitempty"`
}

// Refusal records an issue the publisher declined to label.
type Refusal struct {
	Key     string `json:"key"`
	IssueID string `json:"issueId"`
	Reason  string `json:"reason"`
}

// Result is Publish's outcome.
type Result struct {
	Digest     string
	Filed      []FiledIssue
	Suppressed []Suppression
	Overflow   []string
	Refused    []Refusal
	Annotated  []string
}

// Approved counts filed issues that carry goobers:approved.
func (r Result) Approved() int {
	n := 0
	for _, f := range r.Filed {
		if f.Approved {
			n++
		}
	}
	return n
}

func (p Publisher) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p Publisher) check(artifact Artifact) (string, error) {
	if p.Provider == nil || p.Repo.Name == "" || p.RunID == "" {
		return "", fmt.Errorf("publisher provider, repository, and run id are required")
	}
	if err := p.Policy.validate(); err != nil {
		return "", err
	}
	if v := Validate(artifact, p.RunID); !v.Valid {
		return "", fmt.Errorf("nominations artifact is invalid: %s", strings.Join(v.Errors, "; "))
	}
	return Digest(artifact)
}

// Scan runs the deterministic dedupe and budget pass without mutating
// anything: it lists the issues carrying the nominated label that can still
// suppress a candidate — every open one, and the closed ones updated inside
// the dedupe window — plus every flake-watch issue, and decides what Publish
// would file.
func (p Publisher) Scan(ctx context.Context, artifact Artifact) (Plan, error) {
	digest, err := p.check(artifact)
	if err != nil {
		return Plan{}, err
	}
	cutoff := p.now().Add(-p.Policy.DedupeWindow)
	nominated, err := p.listNominated(ctx, cutoff)
	if err != nil {
		return Plan{}, err
	}
	// Flake-watch's own ledger (test/flakewatch) lists state=all: a closed
	// flake issue still owns its fingerprint, so this arm is not windowed.
	flakes, err := p.Provider.ListWorkItems(ctx, providers.ListWorkItemsRequest{
		Repository: p.Repo, State: "all", Labels: []string{FlakeLabel},
	})
	if err != nil {
		return Plan{}, fmt.Errorf("list flake-watch issues: %w", err)
	}
	byKey := make(map[string][]providers.WorkItem, len(nominated))
	byFinding := make(map[string][]providers.WorkItem)
	for _, item := range nominated {
		if hash, ok := ParseKeyMarker(item.Body); ok {
			byKey[hash] = append(byKey[hash], item)
		}
		for _, hash := range ParseFindingMarkers(item.Body) {
			byFinding[hash] = append(byFinding[hash], item)
		}
	}
	artifactKeys := make(map[string]bool, len(artifact.Nominations))
	for _, n := range artifact.Nominations {
		artifactKeys[KeyHash(n.DedupeKey)] = true
	}
	byFingerprint := make(map[string]providers.WorkItem, len(flakes))
	for _, item := range flakes {
		if fp, ok := ParseFlakeFingerprint(item.Body); ok {
			byFingerprint[fp] = item
		}
	}

	plan := Plan{Digest: digest, openDuplicates: map[string]Candidate{}, existingIDs: map[string]bool{}}
	for _, item := range nominated {
		plan.existingIDs[item.ID] = true
	}
	for _, item := range flakes {
		plan.existingIDs[item.ID] = true
	}
	var admitted []Candidate
	for _, n := range artifact.Nominations {
		cand := Candidate{Nomination: n, KeyHash: KeyHash(n.DedupeKey)}
		cand.Findings, cand.Strength = p.matchFindings(n)
		if n.TestFailure != nil {
			fp := flake.Fingerprint(n.TestFailure.Package, n.TestFailure.Test, flake.NormalizeSignature(n.TestFailure.Signature))
			if owner, owned := byFingerprint[fp]; owned {
				plan.Suppressed = append(plan.Suppressed, Suppression{
					Key: n.Key, IssueID: owner.ID,
					Reason: fmt.Sprintf("flake-watch already owns fingerprint %s (issue #%s carries %s)", fp, owner.ID, FlakeLabel),
				})
				continue
			}
		}
		var suppressed *Suppression
		for _, prior := range sortedByID(byKey[cand.KeyHash]) {
			if hasFiledMarker(prior.Body, cand.KeyHash, p.RunID) {
				// This run already filed it (a retried attempt): not a
				// duplicate — filing reads it back and makes its label set
				// whole.
				if cand.OwnedIssue == "" {
					cand.OwnedIssue = prior.ID
				}
				continue
			}
			if suppressed != nil {
				continue
			}
			switch {
			case prior.State != "closed":
				cand.OpenDuplicate = prior.ID
				suppressed = &Suppression{Key: n.Key, IssueID: prior.ID, Reason: fmt.Sprintf("open issue #%s carries the same nomination key", prior.ID)}
			case p.Policy.DedupeWindow > 0 && prior.UpdatedAt != nil && !prior.UpdatedAt.Before(cutoff):
				suppressed = &Suppression{Key: n.Key, IssueID: prior.ID, Reason: fmt.Sprintf("issue #%s with the same nomination key closed inside the dedupe window", prior.ID)}
			}
		}
		if suppressed != nil {
			plan.Suppressed = append(plan.Suppressed, *suppressed)
			if cand.OpenDuplicate != "" {
				plan.openDuplicates[n.Key] = cand
			}
			continue
		}
		admitted = append(admitted, cand)
	}
	sort.SliceStable(admitted, func(i, j int) bool {
		if admitted[i].Strength != admitted[j].Strength {
			return admitted[i].Strength > admitted[j].Strength
		}
		return admitted[i].Nomination.Key < admitted[j].Nomination.Key
	})
	// The approval bounds are evaluated in filing order, after the budget
	// cut: a finding is claimed by the first filed candidate that names it,
	// so two nominations sharing one finding approve at most one — the same
	// rule the finding markers on prior issues enforce across runs.
	priors := priorFindings{cutoff: cutoff, byFinding: byFinding, artifactKeys: artifactKeys, claimed: map[string]string{}}
	for i, cand := range admitted {
		if i >= p.Policy.MaxPerRun {
			plan.Overflow = append(plan.Overflow, cand.Nomination.Key)
			continue
		}
		cand.ApprovalUnmet = p.approvalUnmet(cand, priors)
		priors.claim(cand.Nomination)
		plan.File = append(plan.File, cand)
	}
	return plan, nil
}

// priorFindings is what the duplicate bound is checked against: the prior
// nominated issues carrying each finding marker (open, or closed inside the
// dedupe window — the bounded listing sees nothing else), the keys of the
// artifact being filed (so this run's own filings of it are told apart from
// every other issue), and the findings the candidates already filed in this
// scan have claimed.
type priorFindings struct {
	cutoff       time.Time
	byFinding    map[string][]providers.WorkItem
	artifactKeys map[string]bool
	claimed      map[string]string
}

// claim records every finding a filed candidate names, first filer wins.
func (pf priorFindings) claim(n Nomination) {
	for _, e := range n.Evidence {
		if e.Kind != EvidenceFinding {
			continue
		}
		hash := FindingHash(findingOf(e))
		if _, taken := pf.claimed[hash]; !taken {
			pf.claimed[hash] = n.Key
		}
	}
}

// listNominated bounds the dedupe listing so an attempt's cost does not grow
// with every nominated issue the repository has ever accumulated: the open
// arm is unbounded because an open match suppresses at any age, and the
// closed arm is windowed by UpdatedSince because a closed match outside the
// dedupe window never changes the outcome (a zero window skips it entirely).
func (p Publisher) listNominated(ctx context.Context, cutoff time.Time) ([]providers.WorkItem, error) {
	open, err := p.Provider.ListWorkItems(ctx, providers.ListWorkItemsRequest{
		Repository: p.Repo, State: "open", Labels: []string{p.Policy.NominatedLabel},
	})
	if err != nil {
		return nil, fmt.Errorf("list open nominated issues: %w", err)
	}
	if p.Policy.DedupeWindow <= 0 {
		return open, nil
	}
	closed, err := p.Provider.ListWorkItems(ctx, providers.ListWorkItemsRequest{
		Repository: p.Repo, State: "closed", Labels: []string{p.Policy.NominatedLabel}, UpdatedSince: &cutoff,
	})
	if err != nil {
		return nil, fmt.Errorf("list nominated issues closed inside the dedupe window: %w", err)
	}
	return append(open, closed...), nil
}

// Publish files the scan's admitted candidates. Every create carries
// RunID "nomination-<keyHash>-<runId>", so a retried attempt returns the
// original issue instead of a second one — the budget is retry-safe, not
// advisory. The run id is part of the key on purpose: across runs the body
// marker decides, and a key whose prior issue closed outside the dedupe
// window must file a fresh issue rather than be stitched onto the closed one.
func (p Publisher) Publish(ctx context.Context, artifact Artifact) (Result, error) {
	plan, err := p.Scan(ctx, artifact)
	if err != nil {
		return Result{}, err
	}
	result := Result{Digest: plan.Digest, Suppressed: plan.Suppressed, Overflow: plan.Overflow}
	for _, cand := range plan.File {
		filed, refusal, err := p.file(ctx, artifact, cand, plan.existingIDs)
		if err != nil {
			return result, err
		}
		if refusal != nil {
			result.Refused = append(result.Refused, *refusal)
			continue
		}
		result.Filed = append(result.Filed, filed)
	}
	keys := make([]string, 0, len(plan.openDuplicates))
	for key := range plan.openDuplicates {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		cand := plan.openDuplicates[key]
		annotated, err := p.annotateOpenDuplicate(ctx, cand)
		if err != nil {
			return result, err
		}
		if annotated {
			result.Annotated = append(result.Annotated, cand.OpenDuplicate)
		}
	}
	return result, nil
}

func (p Publisher) file(ctx context.Context, artifact Artifact, cand Candidate, existingIDs map[string]bool) (FiledIssue, *Refusal, error) {
	n := cand.Nomination
	needsHuman := n.RiskClass == RiskHuman || n.RequiresHumanReview
	labels := p.baseLabels(n, needsHuman)
	var item providers.WorkItem
	var err error
	if cand.OwnedIssue != "" {
		item, err = p.Provider.GetWorkItem(ctx, p.Repo, cand.OwnedIssue)
		if err != nil {
			return FiledIssue{}, nil, fmt.Errorf("read issue #%s this run filed for nomination %q: %w", cand.OwnedIssue, n.Key, err)
		}
	} else {
		item, err = p.Provider.CreateWorkItem(ctx, providers.CreateWorkItemRequest{
			Repository: p.Repo,
			Title:      n.Title,
			Body:       IssueBody(cand.KeyHash, p.RunID, artifact.Producer, n, needsHuman),
			Labels:     labels,
			RunID:      CreateRunID(cand.KeyHash, p.RunID),
		})
		if err != nil {
			return FiledIssue{}, nil, fmt.Errorf("create issue for nomination %q: %w", n.Key, err)
		}
	}
	if item.HasLabel(FlakeLabel) {
		return FiledIssue{}, &Refusal{
			Key: n.Key, IssueID: item.ID,
			Reason: fmt.Sprintf("issue #%s carries %s; flake-watch owns it and strips goobers labels, so none are applied", item.ID, FlakeLabel),
		}, nil
	}
	reused := existingIDs[item.ID]
	if missing := missingLabels(item.Labels, labels); len(missing) > 0 {
		// A retry found the original issue; make its label set whole.
		reused = true
		item, err = p.Provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: p.Repo, ID: item.ID, ExpectedRevision: item.Revision, AddLabels: missing,
		})
		if err != nil {
			return FiledIssue{}, nil, fmt.Errorf("label issue #%s for nomination %q: %w", item.ID, n.Key, err)
		}
	}
	filed := FiledIssue{Key: n.Key, IssueID: item.ID, URL: item.URL, Reused: reused, ApprovalUnmet: cand.ApprovalUnmet}
	switch {
	case len(filed.ApprovalUnmet) > 0:
	case !p.Policy.AutoApprove:
		filed.ApprovalUnmet = []string{"autoApprove is never on this stage (set autoApprove: deterministic-only to approve confirmed tool findings)"}
	case p.Approver == nil:
		filed.ApprovalUnmet = []string{"the github:issues:approve credential did not resolve"}
	default:
		if !item.HasLabel(providers.LabelApproved) {
			// The approve label is applied by the capability's own
			// credential — never the write token — so the far-side
			// label-event ledger names the approving identity.
			item, err = p.Approver.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
				Repository: p.Repo, ID: item.ID, ExpectedRevision: item.Revision, AddLabels: []string{providers.LabelApproved},
			})
			if err != nil {
				return FiledIssue{}, nil, fmt.Errorf("approve issue #%s for nomination %q: %w", item.ID, n.Key, err)
			}
		}
		filed.Approved = true
	}
	filed.Labels = append([]string(nil), item.Labels...)
	return filed, nil, nil
}

// baseLabels is the closed set the publisher applies with the write
// credential: backlog, partition and nominated markers, the validated
// area/type labels, and needs-human when the finder or its source finding
// asked for a human. goobers:approved is never in this set — it is applied
// separately, by the approve credential, and only on a confirmed tool
// finding (file). Readiness, priority, claim and partition-sibling labels are
// never applied at all — curation owns them.
func (p Publisher) baseLabels(n Nomination, needsHuman bool) []string {
	labels := []string{p.Policy.BacklogLabel, p.Policy.PartitionLabel, p.Policy.NominatedLabel}
	labels = append(labels, n.Labels...)
	if needsHuman {
		labels = append(labels, providers.LabelNeedsHuman)
	}
	return labels
}

// loadBearingPaths are envelopes a change cannot touch and still be
// auto-approved (decision 004 §2): the API and its schemas, design docs, CI
// workflows, deployment, the provider model, and the journal. A directory
// entry (trailing slash) covers everything under it; a file entry is exact.
var loadBearingPaths = []string{"api/", "api/schemas/", "docs/design/", ".github/workflows/", "deploy/", "providers/model.go", "internal/journal/"}

// matchFindings confirms every finding pointer a nomination carries against
// the tool output. It returns the confirmed findings and the budget
// strength: 2 only when there is at least one finding pointer and every one
// of them matched; one fabricated pointer beside a real one is a fabricated
// nomination.
func (p Publisher) matchFindings(n Nomination) ([]Finding, int) {
	var matched []Finding
	for _, e := range n.Evidence {
		if e.Kind != EvidenceFinding {
			continue
		}
		finding, ok := p.Findings.Match(e)
		if !ok {
			return nil, 1
		}
		matched = append(matched, finding)
	}
	if len(matched) == 0 {
		return nil, 1
	}
	return matched, 2
}

// approvalUnmet evaluates the deterministic approval bounds and names every
// one the candidate fails: a confirmed tool finding (decision 004 §1), then
// the mechanical bounds the brief states (§2) — riskClass low, type:bug, one
// package, no load-bearing path, no needs-human trigger, and no open or
// windowed-closed duplicate. The duplicate bound is keyed on the finding,
// not on the model's dedupeKey: the dedupe scan suppresses a repeated key
// before this runs, but a prior nominated issue that names the same
// confirmed finding under another key (an issue carrying its finding
// marker), or an earlier candidate of this scan that names it, is the same
// defect, and only one issue per finding may be approved. The opt-in and the
// approve credential are checked at filing time (file), so a --check reports
// what the write would approve.
func (p Publisher) approvalUnmet(cand Candidate, priors priorFindings) []string {
	n := cand.Nomination
	var unmet []string
	switch {
	case p.Findings == nil:
		reason := p.FindingsUnavailable
		if reason == "" {
			reason = "the collect-repo-signals stdout artifact is not readable from this stage"
		}
		unmet = append(unmet, "no tool finding can be confirmed: "+reason)
	case len(cand.Findings) == 0:
		named := false
		for i, e := range n.Evidence {
			if e.Kind != EvidenceFinding {
				continue
			}
			named = true
			if _, ok := p.Findings.Match(e); !ok {
				unmet = append(unmet, fmt.Sprintf("evidence %d names a %s finding the collect-repo-signals artifact of this run does not contain", i, e.Tool))
			}
		}
		if !named {
			unmet = append(unmet, "no evidence names a deterministic tool finding (kind finding)")
		}
	}
	if n.RiskClass != RiskLow {
		unmet = append(unmet, fmt.Sprintf("riskClass is %q, not low", n.RiskClass))
	}
	if !slices.Contains(n.Labels, "type:bug") {
		unmet = append(unmet, "labels do not include type:bug (only a defect a tool reported is approvable)")
	}
	if len(cand.Findings) > 0 {
		unmet = append(unmet, fixSurfaceUnmet(n, cand.Findings)...)
		unmet = append(unmet, p.duplicateFindingUnmet(cand.Findings, priors)...)
	}
	if n.RequiresHumanReview {
		unmet = append(unmet, "the source finding requires human review")
	}
	return unmet
}

// duplicateFindingUnmet names, for every confirmed finding, the prior issue
// or the earlier candidate of this scan that already names it. An issue this
// run filed for a nomination of this artifact is not a prior: it is the
// candidate's own retry read-back, or a sibling the claim order already
// decided.
func (p Publisher) duplicateFindingUnmet(findings []Finding, priors priorFindings) []string {
	var unmet []string
	for _, f := range findings {
		hash := FindingHash(f)
		if key, claimed := priors.claimed[hash]; claimed {
			unmet = append(unmet, fmt.Sprintf("nomination %q of this artifact already names finding %s (at most one issue per finding is approved)", key, f))
			continue
		}
		for _, prior := range sortedByID(priors.byFinding[hash]) {
			if filedByRunForKeys(prior.Body, p.RunID, priors.artifactKeys) {
				continue
			}
			switch {
			case prior.State != "closed":
				unmet = append(unmet, fmt.Sprintf("open issue #%s already names finding %s", prior.ID, f))
			case p.Policy.DedupeWindow > 0 && prior.UpdatedAt != nil && !prior.UpdatedAt.Before(priors.cutoff):
				unmet = append(unmet, fmt.Sprintf("issue #%s naming finding %s closed inside the dedupe window", prior.ID, f))
			default:
				continue
			}
			break
		}
	}
	return unmet
}

// modulePath is the Go module the collect-repo-signals stage runs the tools
// over — the repository the filer's bounds (loadBearingPaths) are written
// for. A go test finding names its package by import path; its
// repository-relative directory, the fix surface, is the import path with
// the module path stripped. A package outside the module has no directory
// in the repository and no fix surface here.
const modulePath = "github.com/goobers/goobers"

// fixSurfaceUnmet checks the "one bounded fix surface" bound: every confirmed
// finding lies in one package, every source pointer lies in that package,
// and none of it is load-bearing. A vet or lint finding's package is its
// file's directory; a test finding's package is its import path resolved
// to the repository directory it names, which a source pointer's directory
// must equal.
func fixSurfaceUnmet(n Nomination, findings []Finding) []string {
	var unmet []string
	surface := ""
	for _, f := range findings {
		pkg, ok := findingPackageDir(f)
		if !ok {
			unmet = append(unmet, fmt.Sprintf("finding %s names package %q outside module %s, which has no fix surface in this repository", f, f.Package, modulePath))
			continue
		}
		switch {
		case surface == "":
			surface = pkg
		case pkg != surface:
			unmet = append(unmet, fmt.Sprintf("the confirmed findings span more than one package (%s, %s)", surface, pkg))
		}
		if packageDirTouchesLoadBearing(f, pkg) {
			unmet = append(unmet, fmt.Sprintf("finding %s touches a load-bearing path", f))
		}
	}
	for _, e := range n.Evidence {
		if e.Kind != EvidenceSource {
			continue
		}
		if surface != "" && path.Dir(e.Path) != surface {
			unmet = append(unmet, fmt.Sprintf("source evidence %q is outside the finding's package %q", e.Path, surface))
		}
		if touchesLoadBearing(e.Path) {
			unmet = append(unmet, fmt.Sprintf("source evidence touches load-bearing path %q", e.Path))
		}
	}
	return slices.Compact(unmet)
}

// findingPackageDir is the repository-relative directory a finding's fix
// surface is: the file's directory for vet and lint, the import path with
// the module path stripped for a test finding (false when the import path
// is not in the module).
func findingPackageDir(f Finding) (string, bool) {
	if f.Tool != ToolTest {
		return path.Dir(f.Path), true
	}
	return packageDir(f.Package)
}

// packageDir resolves an import path to its directory in the repository:
// the module root is ".", and anything not under the module has none.
func packageDir(importPath string) (string, bool) {
	switch {
	case importPath == modulePath:
		return ".", true
	case strings.HasPrefix(importPath, modulePath+"/"):
		dir := strings.TrimPrefix(importPath, modulePath+"/")
		return dir, path.Clean(dir) == dir && dir != "." && dir != ".." && !strings.HasPrefix(dir, "../")
	default:
		return "", false
	}
}

// packageDirTouchesLoadBearing reports whether a finding's fix surface is
// load-bearing: a vet or lint finding's file is; a test finding's whole
// package directory is, when the directory lies in a load-bearing envelope
// or a load-bearing entry lies inside it.
func packageDirTouchesLoadBearing(f Finding, dir string) bool {
	if f.Tool != ToolTest {
		return touchesLoadBearing(f.Path)
	}
	if touchesLoadBearing(dir) {
		return true
	}
	for _, entry := range loadBearingPaths {
		if dir == "." || strings.HasPrefix(entry, dir+"/") {
			return true
		}
	}
	return false
}

func touchesLoadBearing(p string) bool {
	for _, entry := range loadBearingPaths {
		if strings.HasSuffix(entry, "/") {
			if p == strings.TrimSuffix(entry, "/") || strings.HasPrefix(p, entry) {
				return true
			}
		} else if p == entry {
			return true
		}
	}
	return false
}

func (p Publisher) annotateOpenDuplicate(ctx context.Context, cand Candidate) (bool, error) {
	marker := SeenMarker(cand.KeyHash, p.RunID)
	comments, err := p.Provider.ListComments(ctx, p.Repo, cand.OpenDuplicate)
	if err != nil {
		return false, fmt.Errorf("list comments on #%s: %w", cand.OpenDuplicate, err)
	}
	for _, c := range comments {
		if hasSeenMarker(c.Body, cand.KeyHash, p.RunID) {
			return false, nil
		}
	}
	body := fmt.Sprintf("Nominated again by run `%s` as `%s`: %s\n\n%s", p.RunID, cand.Nomination.Key, cand.Nomination.Title, marker)
	if _, err := p.Provider.CreateWorkItemComment(ctx, p.Repo, cand.OpenDuplicate, body); err != nil {
		return false, fmt.Errorf("comment on #%s: %w", cand.OpenDuplicate, err)
	}
	return true, nil
}

// IssueBody renders the body the publisher writes: the key marker first,
// then the nomination, its evidence, its risk, and the filing run — as a
// human-readable line and as the filed marker a retried attempt reads back.
// runID is the publisher's own run, never a value from the artifact.
func IssueBody(keyHash, runID string, producer Producer, n Nomination, needsHuman bool) string {
	var b strings.Builder
	b.WriteString(KeyMarker(keyHash))
	b.WriteString("\n\n")
	b.WriteString(strings.TrimSpace(n.Body))
	b.WriteString("\n\n## Evidence\n\n")
	for _, e := range n.Evidence {
		switch e.Kind {
		case EvidenceJournal:
			fmt.Fprintf(&b, "- journal: run `%s` seq %d\n", e.RunID, e.Seq)
		case EvidenceArtifact:
			fmt.Fprintf(&b, "- artifact: `%s` (`%s`)\n", e.Path, e.Digest)
		case EvidenceSource:
			if e.Line > 0 {
				fmt.Fprintf(&b, "- source: `%s:%d`\n", e.Path, e.Line)
			} else {
				fmt.Fprintf(&b, "- source: `%s`\n", e.Path)
			}
		case EvidenceFinding:
			if e.Tool == ToolTest {
				fmt.Fprintf(&b, "- finding: %s `%s` `%s`\n", e.Tool, e.Package, e.Test)
			} else {
				fmt.Fprintf(&b, "- finding: %s `%s:%d` %s\n", e.Tool, e.Path, e.Line, e.Rule)
			}
		}
	}
	if n.TestFailure != nil {
		fmt.Fprintf(&b, "- test: `%s` `%s`\n", n.TestFailure.Package, n.TestFailure.Test)
	}
	fmt.Fprintf(&b, "\n## Risk\n\n`%s` — %s\n", n.RiskClass, strings.TrimSpace(n.RiskReason))
	if needsHuman {
		fmt.Fprintf(&b, "\nFor the human: %s\n", strings.TrimSpace(n.RiskReason))
	}
	fmt.Fprintf(&b, "\nNominated by run `%s` (stage `%s`, attempt %d).\n\n%s", runID, producer.Stage, producer.Attempt, FiledMarker(keyHash, runID))
	// One finding marker per finding pointer, computed from the pointer's
	// exact tuple: the identity the duplicate approval bound is keyed on in
	// later runs. A model-authored field cannot carry one (control text).
	seen := map[string]bool{}
	for _, e := range n.Evidence {
		if e.Kind != EvidenceFinding {
			continue
		}
		hash := FindingHash(findingOf(e))
		if !seen[hash] {
			seen[hash] = true
			b.WriteString("\n" + FindingMarker(hash))
		}
	}
	return b.String()
}

// CreateRunID is the providers.CreateWorkItemRequest.RunID the publisher
// stamps into every create, keyed by nomination and run.
func CreateRunID(keyHash, runID string) string {
	return "nomination-" + keyHash + "-" + runID
}

func missingLabels(have, want []string) []string {
	var missing []string
	for _, label := range want {
		if !slices.Contains(have, label) {
			missing = append(missing, label)
		}
	}
	return missing
}

// sortedByID orders prior issues earliest-first by their numeric id (GitHub
// issue numbers), falling back to a string compare for a non-numeric id, so
// "the earliest prior issue" names #9 before #10 — that ordering picks the
// owned issue a retry reads back and the issue a suppression reason names.
func sortedByID(items []providers.WorkItem) []providers.WorkItem {
	out := append([]providers.WorkItem(nil), items...)
	sort.SliceStable(out, func(i, j int) bool {
		a, aErr := strconv.Atoi(out[i].ID)
		b, bErr := strconv.Atoi(out[j].ID)
		if aErr == nil && bErr == nil {
			return a < b
		}
		return out[i].ID < out[j].ID
	})
	return out
}
