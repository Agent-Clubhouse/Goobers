package v1alpha1

// This file defines the V0 stage contract: the three canonical wire envelopes
// every stage executor and the runner exchange at runtime. They are plain Go
// types (JSON-tagged) — NOT CRDs — so the runner, stage executors, providers,
// and gate evaluators can import them without pulling in CRD machinery. JSON
// Schemas in api/schemas/ mirror these shapes and are the cross-language,
// closed (additionalProperties:false) contract. See docs/stage-contract.md.
//
// Terminology: a "stage" (ARCHITECTURE.md §5) is what the workflow/task types
// call a "task"; the terms are equivalent. Field names keep the task-flavored
// spelling (taskId, ...) already used by the workflow definition and compiler.
//
// Load-bearing invariant (ARCHITECTURE.md §2.4): stages communicate ONLY through
// envelopes and artifact pointers. No stage reaches into another stage's state.
// The invocation envelope therefore carries context *pointers* (see ContextPointer
// in artifact.go) — never the result bodies of upstream stages.

// StageContractVersion identifies the version of the stage contract these types
// and the api/schemas/*.schema.json documents implement. The schemas are closed:
// unknown fields are a validation error, and additive changes bump this version.
// v1alpha7 adds input-integrity grades to invocations, backlog items, context
// pointers, and artifacts. v1alpha8 adds InvocationEnvelope.CheckoutCones (#649).
const StageContractVersion = "v1alpha8"

// ---------------------------------------------------------------------------
// Invocation envelope — what the runner hands a stage when the workflow advances.
// ---------------------------------------------------------------------------

// InvocationEnvelope is the standard context block delivered to a stage at
// invocation (agentic via the harness adapter, or to a deterministic stage
// runner). It carries the goal, the isolated workspace, read-only context
// pointers, the declared capability grants, and the stage's static config.
//
// It deliberately carries NO upstream result bodies: a stage consumes prior work
// only by resolving ContextPointers read-only from the journal (§2.4). This makes
// cross-stage state reach-through impossible by construction.
//
// This is a runtime wire envelope, not a Kubernetes object, so it is excluded
// from controller-gen DeepCopy generation (its free-form Inputs map cannot be
// deep-copied generically).
// +kubebuilder:object:generate=false
type InvocationEnvelope struct {
	// TaskID identifies this stage instance within the run.
	TaskID string `json:"taskId"`
	// Attempt identifies the stage attempt so adapter provenance remains
	// retry-safe across local and distributed runners.
	Attempt int `json:"attempt,omitempty"`
	// WorkflowID identifies the workflow definition being executed.
	WorkflowID string `json:"workflowId"`
	// RunID identifies this run (the OpenTelemetry trace id for the run).
	RunID string `json:"runId"`
	// TriggerRef identifies the event or item that caused the run. It is bounded
	// scheduler metadata, not the provider's raw trigger payload.
	TriggerRef string `json:"triggerRef,omitempty"`
	// Gaggle is the gaggle this run belongs to.
	Gaggle string `json:"gaggle"`
	// BranchNamespace is the gaggle's configured run-branch namespace root
	// (GaggleSpec.BranchNamespace, providers.DefaultBranchNamespace when
	// unset). The runner sets it from the same value it names the run branch
	// with, and the executor injects it as GOOBERS_BRANCH_NAMESPACE so a
	// goobers-CLI stage's PR-selector defaults and run-branch head derivation
	// stay aligned with the branch namespace the mirror-fetch exclusion
	// preserves (#965/#1010). Empty means the default namespace.
	BranchNamespace string `json:"branchNamespace,omitempty"`
	// BaseBranch is the gaggle's configured default branch (GaggleSpec.Project.
	// Branch/RepoRef.Branch, "main" when unset). The runner sets it from the
	// same branch every worktree is forked from, and the executor injects it
	// as GOOBERS_BASE_BRANCH so a goobers-CLI PR-lifecycle stage's "base"
	// default agrees with the gaggle's actual branch instead of assuming
	// "main" (#2087). Empty means the default branch.
	BaseBranch string `json:"baseBranch,omitempty"`
	// Goal is the intended outcome of this stage (from the stage definition).
	// Goober names the goober this invocation is for, empty for a
	// deterministic stage or an automated gate. The local runner never needed
	// it on the wire — it dispatches from the workflow Definition and passes
	// the name to its executor factory directly. A Temporal worker has only
	// the envelope, so without this the agentic seam cannot know WHICH goober
	// to construct: invoke.Goober.Invoke(ctx, env) is the whole signature.
	Goober string `json:"goober,omitempty"`
	// Goal is the stage's goal statement.
	Goal string `json:"goal"`
	// InstructionAddendum is an operator-supplied, one-off addition to the
	// agent's instructions for this invocation. It is never part of the workflow
	// definition and is empty for ordinary invocations.
	InstructionAddendum string `json:"instructionAddendum,omitempty"`
	// Workspace is the absolute path to the fresh, isolated, disposable working
	// copy (§5) this stage runs in. The runner guarantees it exists.
	Workspace string `json:"workspace"`
	// RepoRef is the target repository for this run.
	RepoRef RepoRef `json:"repoRef"`
	// AdditionalWorkspaces are read-only checkouts of the gaggle's reference
	// repos (GaggleSpec.AdditionalRepos, MGV-11 #1286): the stage may READ them
	// for cross-repo context, but no push credential is ever provisioned for
	// them, so they are read-only by construction. Empty for a gaggle with no
	// AdditionalRepos. Each is surfaced to the stage subprocess as
	// GOOBERS_ADDITIONAL_REPO_<UPPER_SANITIZED_NAME>=<absolute path>.
	AdditionalWorkspaces []AdditionalWorkspace `json:"additionalWorkspaces,omitempty"`
	// CheckoutCones declares, for each workspace whose checkout is a sparse
	// cone-mode checkout (project.checkout.sparse, #649), the repo-relative
	// path cones it materializes — keyed by workspace identity: "" for the
	// primary Workspace, else the matching AdditionalWorkspaces[i].Name. A
	// workspace absent from this map (the common case) has a full checkout.
	// Deliberately separate from RepoRef.Checkout, which stays off the wire
	// (RepoRef.EnvelopeRef) so the closed repoRef schema never changes — this
	// is an additive envelope-level field instead, so a partial checkout is
	// declared to the stage without depending on a stage ever reading
	// RepoRef.Checkout. Populated so an agentic stage knows the tree is
	// partial and does not "fix" apparently-missing files or misread a
	// pruned path as deleted.
	CheckoutCones map[string][]string `json:"checkoutCones,omitempty"`
	// Item is the backlog item / trigger payload that started the run. Nil for
	// schedule/signal-triggered runs with no originating item. It is a bounded
	// provider-neutral descriptor, not another stage's state; the authoritative,
	// content-digested snapshot is a ContextPointer into the journal's inputs/.
	Item *BacklogItem `json:"item,omitempty"`
	// ContextPointers are the read-only inputs this stage may consume: journal
	// artifact pointers (upstream outputs, input snapshots) and external refs
	// (e.g. issue/PR URLs). Pointers only — never upstream result bodies.
	ContextPointers []ContextPointer `json:"contextPointers,omitempty"`
	// MinimumIntegrity is the lowest provenance grade this stage accepts.
	// The runner enforces it before dispatch and journals a typed refusal.
	MinimumIntegrity Integrity `json:"minimumIntegrity,omitempty"`
	// Capabilities are the capability grants declared by this stage's definition
	// (e.g. "github:issues:write", "repo:push"). Undeclared use fails closed:
	// credentials for capabilities not listed here are never materialized (§5).
	Capabilities []string `json:"capabilities,omitempty"`
	// Limits bound this stage's execution (duration/tokens/cost).
	Limits Limits `json:"limits"`
	// Inputs are the stage's static config from its definition (plus any values
	// the compiler resolved for it). This is the stage's own config, not another
	// stage's runtime state.
	Inputs map[string]interface{} `json:"inputs,omitempty"`
}

// AdditionalWorkspace is one read-only reference-repo checkout handed to a stage
// alongside its primary Workspace (MGV-11 #1286). Name is the reference repo's
// name (from GaggleSpec.AdditionalRepos), Path is the absolute on-disk location
// of its checkout. The stage may read Path but has no credential to push to it.
type AdditionalWorkspace struct {
	// Name is the reference repo's name (GaggleSpec.AdditionalRepos[i].Name).
	Name string `json:"name"`
	// Path is the absolute path to the reference repo's read-only checkout.
	Path string `json:"path"`
}

// BacklogItem is a provider-neutral mirror of a unit of work. The backlog
// remains the source of truth; this is the snapshot handed to a run.
type BacklogItem struct {
	// ID is the provider-native item id (issue number, work-item id).
	ID string `json:"id"`
	// Provider is the backing system the item came from.
	Provider Provider `json:"provider"`
	// Title is the item's short title.
	Title string `json:"title,omitempty"`
	// Body is the item's description/details.
	Body string `json:"body,omitempty"`
	// URL links back to the item in the provider.
	URL string `json:"url,omitempty"`
	// Labels are the item's labels (used by workflow selectors for routing).
	Labels []string `json:"labels,omitempty"`
	// Integrity records whether the provider content was maintainer-approved or
	// remains arbitrary, unapproved input.
	Integrity Integrity `json:"integrity,omitempty"`
}

// Limits bound a stage's execution. Zero values mean "no explicit limit".
type Limits struct {
	// MaxDurationSeconds caps wall-clock time for the stage.
	// +kubebuilder:validation:Minimum=0
	MaxDurationSeconds int32 `json:"maxDurationSeconds,omitempty"`
	// MaxTokens caps model tokens the run may consume.
	// +kubebuilder:validation:Minimum=0
	MaxTokens int64 `json:"maxTokens,omitempty"`
	// MaxCostUSD caps the run's spend.
	// +kubebuilder:validation:Minimum=0
	MaxCostUSD float64 `json:"maxCostUSD,omitempty"`
}

// ---------------------------------------------------------------------------
// Result envelope — what a stage returns.
// ---------------------------------------------------------------------------

// ResultStatus is the terminal status of a stage.
type ResultStatus string

const (
	// ResultSuccess means the stage met its goal; the runner advances.
	ResultSuccess ResultStatus = "success"
	// ResultFailure means the stage did not meet its goal; the runner applies the
	// stage's retry policy and, if exhausted, branches on failure.
	ResultFailure ResultStatus = "failure"
	// ResultBlocked means the stage cannot proceed without external intervention
	// (human input, an unmet dependency); the runner halts the run pending it.
	ResultBlocked ResultStatus = "blocked"
	// ResultNoWork means the stage ran without error but found nothing to act
	// on (issue #233: an empty-backlog claim tick) — distinct from
	// ResultSuccess (which implies the stage actually produced work for a
	// downstream stage to consume) and ResultFailure (which implies a retry
	// policy should apply). The runner short-circuits a ResultNoWork task
	// straight to a clean PhaseCompleted, regardless of the task's declared
	// Next — an agentic downstream stage is never invoked with no subject
	// (ARCHITECTURE.md's "don't fake a success that then does nothing
	// useful" principle). A stage that genuinely errored (a provider/auth
	// failure, a malformed query) must still return ResultFailure, not
	// ResultNoWork — this status is only for "correctly found nothing," the
	// steady state of an idle instance, never a masked error.
	ResultNoWork ResultStatus = "no-work"
)

// ResultEnvelope is the standard stage result the runner acts on, and that gates
// and telemetry consume. Bulk outputs are written into the journal and returned
// as ArtifactPointers; Outputs carries only small declared scalar values.
//
// Runtime wire envelope, not a Kubernetes object — excluded from controller-gen
// DeepCopy generation (its free-form Outputs map cannot be deep-copied).
// +kubebuilder:object:generate=false
type ResultEnvelope struct {
	// Status is the terminal status of the stage.
	Status ResultStatus `json:"status"`
	// Outputs are small, named scalar values downstream stages/gates can consume
	// directly. Anything larger than a scalar is an artifact, referenced by
	// pointer — state does not travel through Outputs.
	Outputs map[string]interface{} `json:"outputs,omitempty"`
	// Artifacts are the stage's produced outputs, each a journal-relative pointer
	// (path + sha256 digest). Downstream stages receive these as ContextPointers.
	Artifacts []ArtifactPointer `json:"artifacts,omitempty"`
	// Transcript points at the runner-captured, scrubbed transcript for this
	// agentic attempt. It is separate from produced Artifacts because it is
	// diagnostic evidence, not a stage output handed to downstream stages.
	Transcript *ArtifactPointer `json:"transcript,omitempty"`
	// Summary is a human-readable summary of what happened.
	Summary string `json:"summary,omitempty"`
	// Metrics are numeric measures (duration, tokens, cost, custom). Agentic
	// usage uses gen_ai.usage.input_tokens, gen_ai.usage.output_tokens,
	// goobers.usage.copilot_premium_requests, and goobers.usage.cost_usd.
	// Unavailable measures are omitted; observed zeroes remain present.
	Metrics map[string]float64 `json:"metrics,omitempty"`
	// Error carries failure or blockage detail; omitted for success and no-work.
	Error *ErrorInfo `json:"error,omitempty"`
	// Integrity is the provenance of the content this stage produced — the
	// weakest grade among the inputs it was admitted with. Artifacts carry their
	// own labels, but Outputs are bare scalars with nowhere to hang provenance,
	// so a downstream stage resolving inputsFrom grades the producing stage
	// rather than the value. Without it, a stage could refuse an unapproved
	// producer's artifact via contextFrom and still import that producer's
	// provider-authored text through inputsFrom (TBH-4).
	Integrity Integrity `json:"integrity,omitempty"`
}

// ErrorInfo describes a stage failure.
type ErrorInfo struct {
	// Code is a stable, machine-readable error code.
	Code string `json:"code"`
	// Message is the human-readable error message.
	Message string `json:"message"`
	// Retryable indicates whether a retry might succeed (informs the runner's
	// retry decision alongside the stage's declared policy).
	Retryable bool `json:"retryable,omitempty"`
}

// ---------------------------------------------------------------------------
// Verdict — what a gate evaluator returns (§5 gates).
// ---------------------------------------------------------------------------

// VerdictDecision is the outcome of a gate evaluator.
type VerdictDecision string

const (
	// VerdictPass approves.
	VerdictPass VerdictDecision = "pass"
	// VerdictFail rejects.
	VerdictFail VerdictDecision = "fail"
	// VerdictNeedsChanges requests changes before approval.
	VerdictNeedsChanges VerdictDecision = "needs-changes"
)

// Severity ranks a finding.
type Severity string

// Severity levels for reviewer findings, from least to most serious.
const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityError    Severity = "error"
	SeverityCritical Severity = "critical"
)

// Rank orders severities from least to most serious (1-4), for threshold
// comparisons (e.g. pr-remediation's minSeverity policy, issue #941). An
// unknown value ranks below SeverityInfo (0) so a typo in a declared
// threshold fails toward "nothing meets this bar" rather than silently
// matching every finding.
func (s Severity) Rank() int {
	switch s {
	case SeverityInfo:
		return 1
	case SeverityWarning:
		return 2
	case SeverityError:
		return 3
	case SeverityCritical:
		return 4
	default:
		return 0
	}
}

// FindingClass routes a merge-review Finding to the right pr-remediation
// action (issue #358, design docs/design/v0/pr-lifecycle-loop.md §4 D1).
// Empty on an ordinary in-run gate Finding (implementation's reviewer gate,
// etc.) — classes are a PR-lifecycle-altitude concept only merge-review
// populates. CI failures deliberately do not have a finding class: deterministic
// provider check evidence reaches remediation through its separate CI channel.
type FindingClass string

const (
	// FindingRebaseNeeded means the PR's base has advanced; a (possibly
	// clean) rebase is required before anything else.
	FindingRebaseNeeded FindingClass = "rebase-needed"
	// FindingConflict means a rebase does not apply cleanly and needs
	// resolution — this alone makes the finding substantive (D3: routing is
	// finding-driven, never rebase-driven).
	FindingConflict FindingClass = "conflict"
	// FindingSubstantive means a real code change is required: cross-PR
	// drift, a regression, a human/other-agent review comment, or a genuine
	// defect the holistic review caught.
	FindingSubstantive FindingClass = "substantive"
	// FindingMissingTests means behavior lacks the tests needed to establish
	// and preserve its correctness.
	FindingMissingTests FindingClass = "missing-tests"
	// FindingScopeCreep means changes unrelated to the requested work must be
	// removed.
	FindingScopeCreep FindingClass = "scope-creep"
	// FindingContractChange means a load-bearing contract was changed without
	// the requested work authorizing that change.
	FindingContractChange FindingClass = "contract-change"
	// FindingCrossPRBlocked means the PR is correct in isolation but must
	// wait behind another PR (§7 serialization/ordering).
	FindingCrossPRBlocked FindingClass = "cross-pr-blocked"
)

// IsValid reports whether c is a known finding class. The zero value ""
// is deliberately NOT valid here — call sites that care whether a class was
// actually set (vs. an ordinary in-run Finding that never populates one)
// check for empty separately; IsValid is for validating a class that claims
// to be set.
func (c FindingClass) IsValid() bool {
	switch c {
	case FindingRebaseNeeded, FindingConflict, FindingSubstantive, FindingMissingTests,
		FindingScopeCreep, FindingContractChange, FindingCrossPRBlocked:
		return true
	}
	return false
}

// RequiresCodeChange reports whether resolving the finding belongs in the
// existing substantive-remediation lane.
func (c FindingClass) RequiresCodeChange() bool {
	switch c {
	case FindingConflict, FindingSubstantive, FindingMissingTests, FindingScopeCreep, FindingContractChange:
		return true
	}
	return false
}

// Verdict is the structured result a gate evaluator — or, at PR altitude,
// the merge-review workflow (issue #358) — produces. An in-run gate maps
// Decision to a branch; merge-review maps it to a label
// (merge-ready/needs-remediation/merge-escalated) and a checklist
// pr-remediation must clear entirely. Reusing this one type for both
// altitudes is deliberate (design doc §4): pr-remediation consumes a
// merge-review verdict through the exact same evidence-pointer/artifact
// mechanism the in-run reviewer already uses to feed `implement`, with zero
// new plumbing. HeadSHA/BaseSHA are PR-altitude-only (empty on an in-run
// gate Verdict) — see their own doc comments.
type Verdict struct {
	// Decision is the evaluator's outcome; the gate maps it to a branch.
	Decision VerdictDecision `json:"decision"`
	// Rationale explains the decision in prose.
	Rationale string `json:"rationale,omitempty"`
	// Evidence are journal artifact pointers backing the decision.
	Evidence []ArtifactPointer `json:"evidence,omitempty"`
	// Findings enumerate specific issues the evaluator found.
	Findings []Finding `json:"findings,omitempty"`
	// Summary is a human-readable summary of the review.
	Summary string `json:"summary,omitempty"`
	// HeadSHA is the PR head commit this verdict was computed against
	// (design doc §6 D6, SHA-pinning) — empty for an in-run gate Verdict,
	// which has no PR of its own to pin against. Before acting on a
	// merge-ready verdict, the current head/base MUST be re-checked against
	// this pin; a mismatch voids the verdict (it was computed against a
	// state that no longer exists) and forces re-review rather than merging
	// something reviewed against a stale diff.
	HeadSHA string `json:"headSha,omitempty"`
	// BaseSHA is the base branch commit this verdict was computed against —
	// see HeadSHA's doc comment; both pin together, since a PR can go stale
	// via either its own new commits or the base moving.
	BaseSHA string `json:"baseSha,omitempty"`
	// Digest is the reviewDigest (issue #523) this verdict was computed
	// against — a content hash of every input the holistic reviewer saw:
	// the selected PR's head/base SHAs, gate-relevant labels, and the
	// verdict-schema version. Empty for a Verdict that doesn't participate
	// in cross-run digest caching (every gate but merge-review's holistic
	// review). A later run computing the identical digest reuses this verdict
	// verbatim instead of re-invoking the reviewer
	// (gate.Evaluator.CachedVerdict).
	Digest string `json:"digest,omitempty"`
	// SourceRunID is the run whose reviewer evaluation ORIGINALLY produced
	// this verdict — never touched by a cache hit, which reuses the
	// verdict's content, including this field, unchanged. Set once by
	// apply-verdict from its own GOOBERS_RUN_ID at the moment a genuinely
	// fresh (non-cached) verdict is first posted, so a verdict handed back
	// by the cache (whether read from a PR comment or a run's own journal)
	// always still names the run a human or `goobers trace` would need to
	// inspect to see the real reviewer reasoning behind it.
	SourceRunID string `json:"sourceRunId,omitempty"`
	// OverlapCluster records whether this PR shared a deterministic file
	// overlap with at least one other open PR (#989/#990) at the moment this
	// verdict was published — PR-altitude only, always false for an in-run
	// gate Verdict. See Elected.
	OverlapCluster bool `json:"overlapCluster,omitempty"`
	// Elected records whether this PR was the single-lander election's
	// deterministic winner (PRL-021) for its overlap cluster at the moment
	// this verdict was published — always false when OverlapCluster is
	// false. A published `pass` with OverlapCluster true and Elected false
	// is not a landing authority: merge-pr's election conjunct (#1071)
	// refuses to land it, so GitHub's native merge queue can never crown a
	// cluster member on its own.
	Elected bool `json:"elected,omitempty"`
}

// Finding is a single issue raised by an evaluator.
type Finding struct {
	// Severity ranks the finding.
	Severity Severity `json:"severity"`
	// Message describes the issue.
	Message string `json:"message"`
	// Location optionally points at where the issue is (e.g. "path/to/file:42").
	Location string `json:"location,omitempty"`
	// Class routes a merge-review finding to the right pr-remediation action
	// (issue #358) — empty on an ordinary in-run gate Finding. The verdict is
	// a checklist: pr-remediation must clear every classed finding, and
	// merge-review re-verifies every one (SHA-pinned) before merge-ready.
	Class FindingClass `json:"class,omitempty"`
	// BlockingPRs names the sibling PR number(s) a FindingCrossPRBlocked
	// finding is waiting behind (#747) — populated only when
	// Class == FindingCrossPRBlocked. Before this field existed, the only
	// record of *which* PR was blocking was free prose in Message, useless
	// to automated routing/unparking. Empty on every other Class (including
	// the zero value).
	BlockingPRs []int `json:"blockingPrs,omitempty"`
}

// IsValid reports whether f is structurally usable: any set Class must be a
// known one, and a FindingCrossPRBlocked finding must name at least one
// blocker (#747) — a finding claiming "blocked by a sibling" with no known
// sibling is worse than not raising it at all (it can never be resolved by
// an automated unpark), so this fails closed rather than silently accepting
// an unusable record.
func (f Finding) IsValid() bool {
	if f.Class != "" && !f.Class.IsValid() {
		return false
	}
	if f.Class == FindingCrossPRBlocked && len(f.BlockingPRs) == 0 {
		return false
	}
	return true
}

// IsValid reports whether s is a known result status.
func (s ResultStatus) IsValid() bool {
	switch s {
	case ResultSuccess, ResultFailure, ResultBlocked, ResultNoWork:
		return true
	}
	return false
}

// IsValid reports whether d is a known verdict decision.
func (d VerdictDecision) IsValid() bool {
	switch d {
	case VerdictPass, VerdictFail, VerdictNeedsChanges:
		return true
	}
	return false
}
