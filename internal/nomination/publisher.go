package nomination

import (
	"context"
	"fmt"
	"path"
	"slices"
	"sort"
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
// label-event ledger on the far side names the approving identity.
type Approver interface {
	GetWorkItem(context.Context, providers.RepositoryRef, string) (providers.WorkItem, error)
	UpdateWorkItem(context.Context, providers.UpdateWorkItemRequest) (providers.WorkItem, error)
}

// EvidenceVerifier checks evidence pointers against what the publisher can
// itself observe: a run journal event and a stage artifact's content digest.
// A model assertion is never evidence; only a verified pointer counts toward
// auto-approval.
type EvidenceVerifier interface {
	VerifyJournal(runID string, seq uint64) bool
	VerifyArtifact(path, digest string) bool
}

// Policy is the publisher's deterministic label and budget policy, sourced
// from stage inputs — never from the artifact.
type Policy struct {
	// BacklogLabel (`goobers`) and PartitionLabel (the instance's claim
	// partition, e.g. goobers:cloud) are applied to every filed issue so it
	// is visible to this instance's own curation.
	BacklogLabel   string
	PartitionLabel string
	// NominatedLabel marks every filed issue and keys the dedupe scan.
	NominatedLabel string
	// MaxPerRun caps creates per run; the remainder is reported as overflow.
	MaxPerRun int
	// DedupeWindow applies only to CLOSED matches: an open match always
	// suppresses, regardless of age.
	DedupeWindow time.Duration
	// AutoApprove opts the stage into applying goobers:approved to low-risk
	// nominations that clear every precondition. Default off.
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
	// resolve; then no nomination is auto-approved.
	Approver Approver
	// Verifier is nil when nothing is verifiable (no journal, no workspace);
	// then no nomination is auto-approved.
	Verifier EvidenceVerifier
	Repo     providers.RepositoryRef
	RunID    string
	Policy   Policy
	Now      func() time.Time
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
	// Strength orders the budget: 3 artifact-backed, 2 journal-backed, 1
	// source-location-only.
	Strength int
	// PriorIssues are issues that carried the same key at any age; any prior
	// issue blocks auto-approval even when the window admits a re-file.
	PriorIssues []string
	// OpenDuplicate is the open issue that suppressed this candidate, when
	// one did (such candidates are in Plan.Suppressed, not Plan.File).
	OpenDuplicate string
	// OwnedIssue is the issue this run already filed for the key (a retried
	// attempt found it in the dedupe listing), so filing reads it back
	// instead of creating — no reliance on the provider's search index.
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
	if v := Validate(artifact); !v.Valid {
		return "", fmt.Errorf("nominations artifact is invalid: %s", strings.Join(v.Errors, "; "))
	}
	return Digest(artifact)
}

// Scan runs the deterministic dedupe and budget pass without mutating
// anything: it lists every issue carrying the nominated label (state=all)
// and every flake-watch issue, and decides what Publish would file.
func (p Publisher) Scan(ctx context.Context, artifact Artifact) (Plan, error) {
	digest, err := p.check(artifact)
	if err != nil {
		return Plan{}, err
	}
	nominated, err := p.Provider.ListWorkItems(ctx, providers.ListWorkItemsRequest{
		Repository: p.Repo, State: "all", Labels: []string{p.Policy.NominatedLabel},
	})
	if err != nil {
		return Plan{}, fmt.Errorf("list nominated issues: %w", err)
	}
	flakes, err := p.Provider.ListWorkItems(ctx, providers.ListWorkItemsRequest{
		Repository: p.Repo, State: "all", Labels: []string{FlakeLabel},
	})
	if err != nil {
		return Plan{}, fmt.Errorf("list flake-watch issues: %w", err)
	}
	byKey := make(map[string][]providers.WorkItem, len(nominated))
	for _, item := range nominated {
		if hash, ok := ParseKeyMarker(item.Body); ok {
			byKey[hash] = append(byKey[hash], item)
		}
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
	cutoff := p.now().Add(-p.Policy.DedupeWindow)
	for _, n := range artifact.Nominations {
		cand := Candidate{Nomination: n, KeyHash: KeyHash(n.DedupeKey), Strength: evidenceStrength(n)}
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
			if filedByRun(prior.Body, p.RunID) {
				// This run already filed it (a retried attempt): not a
				// duplicate and not a prior — filing reads it back and
				// makes its label set whole.
				if cand.OwnedIssue == "" {
					cand.OwnedIssue = prior.ID
				}
				continue
			}
			cand.PriorIssues = append(cand.PriorIssues, prior.ID)
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
	for i, cand := range admitted {
		if i < p.Policy.MaxPerRun {
			plan.File = append(plan.File, cand)
		} else {
			plan.Overflow = append(plan.Overflow, cand.Nomination.Key)
		}
	}
	return plan, nil
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
			Body:       IssueBody(artifact, cand.KeyHash, n, needsHuman),
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
	filed := FiledIssue{Key: n.Key, IssueID: item.ID, URL: item.URL, Reused: reused}
	filed.ApprovalUnmet = p.approvalUnmet(cand)
	if len(filed.ApprovalUnmet) == 0 {
		if !item.HasLabel(providers.LabelApproved) {
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

// baseLabels is the closed set the publisher applies: backlog, partition and
// nominated markers, the validated area/type labels, and needs-human when the
// finder or its source finding asked for a human. Readiness, priority, claim
// and partition-sibling labels are never in this set — curation owns them.
func (p Publisher) baseLabels(n Nomination, needsHuman bool) []string {
	labels := []string{p.Policy.BacklogLabel, p.Policy.PartitionLabel, p.Policy.NominatedLabel}
	labels = append(labels, n.Labels...)
	if needsHuman {
		labels = append(labels, providers.LabelNeedsHuman)
	}
	return labels
}

// loadBearingPaths are envelopes a change cannot touch and still be
// auto-approved: the API and schemas, design docs, CI workflows, deployment,
// the provider model, and the journal.
var loadBearingPaths = []string{"api/", "docs/design/", ".github/workflows/", "deploy/", "internal/journal/", "providers/model.go"}

// approvalUnmet evaluates the eight auto-approval preconditions and names
// every one that fails. An empty result means goobers:approved is applied.
func (p Publisher) approvalUnmet(cand Candidate) []string {
	n := cand.Nomination
	var unmet []string
	if n.RiskClass != RiskLow {
		unmet = append(unmet, fmt.Sprintf("riskClass is %q, not low", n.RiskClass))
	}
	if !p.Policy.AutoApprove {
		unmet = append(unmet, "autoApprove is not enabled on this stage")
	}
	if p.Approver == nil {
		unmet = append(unmet, "the github:issues:approve credential did not resolve")
	}
	if !p.verifiedEvidence(n) {
		unmet = append(unmet, "no evidence pointer could be verified against a run journal or a stage artifact digest")
	}
	dirs := map[string]bool{}
	for _, e := range n.Evidence {
		if e.Kind == EvidenceSource {
			dirs[path.Dir(e.Path)] = true
		}
	}
	if len(dirs) != 1 {
		unmet = append(unmet, fmt.Sprintf("source evidence must name exactly one directory, names %d", len(dirs)))
	}
	for _, e := range n.Evidence {
		if e.Kind == EvidenceSource && touchesLoadBearing(e.Path) {
			unmet = append(unmet, fmt.Sprintf("source evidence touches load-bearing path %q", e.Path))
			break
		}
	}
	if n.RequiresHumanReview {
		unmet = append(unmet, "the source finding requires human review")
	} else if slices.Contains(n.Labels, "type:feature") {
		unmet = append(unmet, "a type:feature nomination proposes new behaviour, which needs a human")
	}
	if len(cand.PriorIssues) > 0 {
		unmet = append(unmet, fmt.Sprintf("a prior issue carried the same nomination key: #%s", strings.Join(cand.PriorIssues, ", #")))
	}
	return unmet
}

func (p Publisher) verifiedEvidence(n Nomination) bool {
	if p.Verifier == nil {
		return false
	}
	for _, e := range n.Evidence {
		switch e.Kind {
		case EvidenceJournal:
			if p.Verifier.VerifyJournal(e.RunID, e.Seq) {
				return true
			}
		case EvidenceArtifact:
			if p.Verifier.VerifyArtifact(e.Path, e.Digest) {
				return true
			}
		}
	}
	return false
}

func touchesLoadBearing(p string) bool {
	for _, prefix := range loadBearingPaths {
		if p == strings.TrimSuffix(prefix, "/") || strings.HasPrefix(p, prefix) {
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
// then the nomination, its evidence, its risk, and the producing run.
func IssueBody(artifact Artifact, keyHash string, n Nomination, needsHuman bool) string {
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
		}
	}
	if n.TestFailure != nil {
		fmt.Fprintf(&b, "- test: `%s` `%s`\n", n.TestFailure.Package, n.TestFailure.Test)
	}
	fmt.Fprintf(&b, "\n## Risk\n\n`%s` — %s\n", n.RiskClass, strings.TrimSpace(n.RiskReason))
	if needsHuman {
		fmt.Fprintf(&b, "\nFor the human: %s\n", strings.TrimSpace(n.RiskReason))
	}
	b.WriteString("\n" + filedByRunLine(artifact.RunID))
	fmt.Fprintf(&b, " (stage `%s`, attempt %d).", artifact.Producer.Stage, artifact.Producer.Attempt)
	return b.String()
}

// CreateRunID is the providers.CreateWorkItemRequest.RunID the publisher
// stamps into every create, keyed by nomination and run.
func CreateRunID(keyHash, runID string) string {
	return "nomination-" + keyHash + "-" + runID
}

func filedByRunLine(runID string) string {
	return "Nominated by run `" + runID + "`"
}

func filedByRun(body, runID string) bool {
	return strings.Contains(body, filedByRunLine(runID)+" (")
}

func evidenceStrength(n Nomination) int {
	strength := 1
	for _, e := range n.Evidence {
		switch e.Kind {
		case EvidenceArtifact:
			return 3
		case EvidenceJournal:
			strength = 2
		}
	}
	return strength
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

func sortedByID(items []providers.WorkItem) []providers.WorkItem {
	out := append([]providers.WorkItem(nil), items...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
