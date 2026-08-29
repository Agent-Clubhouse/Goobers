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
		cand.ApprovalUnmet = p.approvalUnmet(cand)
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
// package, no load-bearing path, no needs-human trigger. The last §2 bound,
// no open or windowed-closed duplicate, holds by construction: Scan
// suppresses such candidates before this is evaluated, so an admitted
// candidate has none. The opt-in and the approve credential are checked at
// filing time (file), so a --check reports what the write would approve.
func (p Publisher) approvalUnmet(cand Candidate) []string {
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
	}
	if n.RequiresHumanReview {
		unmet = append(unmet, "the source finding requires human review")
	}
	return unmet
}

// fixSurfaceUnmet checks the "one bounded fix surface" bound: every confirmed
// finding lies in one package, every source pointer lies in that package,
// and none of it is load-bearing. A vet or lint finding's package is its
// file's directory; a test finding's package is its import path, which a
// source pointer's directory must be a suffix of.
func fixSurfaceUnmet(n Nomination, findings []Finding) []string {
	var unmet []string
	surface := ""
	for _, f := range findings {
		pkg := findingPackage(f)
		switch {
		case surface == "":
			surface = pkg
		case pkg != surface:
			unmet = append(unmet, fmt.Sprintf("the confirmed findings span more than one package (%s, %s)", surface, pkg))
		}
		if findingTouchesLoadBearing(f) {
			unmet = append(unmet, fmt.Sprintf("finding %s touches a load-bearing path", f))
		}
	}
	for _, e := range n.Evidence {
		if e.Kind != EvidenceSource {
			continue
		}
		if !inPackage(e.Path, surface) {
			unmet = append(unmet, fmt.Sprintf("source evidence %q is outside the finding's package %q", e.Path, surface))
		}
		if touchesLoadBearing(e.Path) {
			unmet = append(unmet, fmt.Sprintf("source evidence touches load-bearing path %q", e.Path))
		}
	}
	return slices.Compact(unmet)
}

func findingPackage(f Finding) string {
	if f.Tool == ToolTest {
		return f.Package
	}
	return path.Dir(f.Path)
}

// inPackage reports whether a source path's directory is the package: equal
// to a directory surface, or a suffix of an import-path surface.
func inPackage(source, surface string) bool {
	dir := path.Dir(source)
	if dir == surface {
		return true
	}
	return dir != "." && strings.HasSuffix(surface, "/"+dir)
}

func findingTouchesLoadBearing(f Finding) bool {
	if f.Tool != ToolTest {
		return touchesLoadBearing(f.Path)
	}
	// A test finding's fix surface is its whole package directory, which is
	// load-bearing when a load-bearing entry lies in or above it: the entry's
	// directory is a suffix of, or a segment run inside, the import path.
	for _, entry := range loadBearingPaths {
		dir := strings.TrimSuffix(entry, "/")
		if !strings.HasSuffix(entry, "/") {
			dir = path.Dir(entry)
		}
		if strings.HasSuffix(f.Package, "/"+dir) || strings.Contains(f.Package, "/"+dir+"/") {
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
