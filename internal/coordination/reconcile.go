package coordination

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/goobers/goobers/providers"
)

// Provider deliberately has no merge, push, deployment or PR mutation methods.
type Provider interface {
	GetWorkItem(context.Context, providers.RepositoryRef, string) (providers.WorkItem, error)
	FindWorkItemsByMarker(context.Context, providers.RepositoryRef, string) ([]providers.WorkItem, error)
	CreateWorkItem(context.Context, providers.CreateWorkItemRequest) (providers.WorkItem, error)
	UpdateWorkItem(context.Context, providers.UpdateWorkItemRequest) (providers.WorkItem, error)
	GetPullRequest(context.Context, providers.RepositoryRef, string) (providers.PullRequestSummary, error)
	GetCoordinationRelease(context.Context, providers.RepositoryRef, string, string) (providers.ReleaseObservation, error)
}

type Binding struct {
	NodeRef
	Issue      string `json:"issue"`
	PR         string `json:"pr,omitempty"`
	HeadSHA    string `json:"headSha,omitempty"`
	MergeSHA   string `json:"mergeSha,omitempty"`
	ReleaseSHA string `json:"releaseSha,omitempty"`
}

type Integration struct {
	Command        string    `json:"command"`
	Passed         bool      `json:"passed"`
	ArtifactSHA256 string    `json:"artifactSha256"`
	Children       []Binding `json:"children"`
}

// Evidence is a maintainer-reviewed attestation, not an autonomous test verdict.
// Its canonical digest must be separately approved in trusted configuration.
type Evidence struct {
	PlanDigest  string       `json:"planDigest"`
	Children    []Binding    `json:"children"`
	Integration *Integration `json:"integration,omitempty"`
}

type ChildResult struct {
	NodeRef
	Issue       string `json:"issue,omitempty"`
	PR          string `json:"pr,omitempty"`
	State       string `json:"state"`
	Reason      string `json:"reason,omitempty"`
	IssueClosed bool   `json:"issueClosed"`
	PRMerged    bool   `json:"prMerged"`
	HeadSHA     string `json:"headSha,omitempty"`
	MergeSHA    string `json:"mergeSha,omitempty"`
	ReleaseTag  string `json:"releaseTag,omitempty"`
	ReleaseSHA  string `json:"releaseSha,omitempty"`
}

type Result struct {
	PlanID                    string        `json:"planId"`
	PlanDigest                string        `json:"planDigest"`
	EvidenceDigest            string        `json:"evidenceDigest,omitempty"`
	IntegrationArtifactSHA256 string        `json:"integrationArtifactSha256,omitempty"`
	State                     string        `json:"state"`
	Reason                    string        `json:"reason,omitempty"`
	Children                  []ChildResult `json:"children"`
}

type Publication struct {
	NodeRef
	Title    string   `json:"title"`
	Body     string   `json:"body"`
	Labels   []string `json:"labels"`
	Assignee string   `json:"assignee,omitempty"`
}

// Publications is the exact initial public content, also usable for manual
// recovery when a durable create intent has an ambiguous provider outcome.
func Publications(p Plan) []Publication {
	digest, _ := Digest(p)
	out := make([]Publication, 0, len(p.Children))
	for _, child := range p.Children {
		out = append(out, Publication{NodeRef: child.NodeRef, Title: child.Title, Body: child.Body + "\n\n" + childMarker(p, child, digest), Labels: []string{providers.LabelCoordinationWait}, Assignee: child.Assignee})
	}
	return out
}

type Reconciler struct {
	Authority Authority
	Providers map[string]Provider
	Leaser    TargetLeaser
	// ArtifactDigest is calculated by the operator CLI over the supplied local
	// integration log; no plan or provider body can choose a local file to read.
	ArtifactDigest string
}

// TargetLeaser is structurally implemented by decomposition.FileTargetLeaser.
type TargetLeaser interface {
	Acquire(context.Context, providers.RepositoryRef, string) (func() error, error)
}

func (r Reconciler) Reconcile(ctx context.Context, p Plan, evidence *Evidence) (out Result, resultErr error) {
	out = Result{PlanID: p.ID, State: "blocked"}
	if err := p.Validate(r.Authority, true); err != nil {
		return out, err
	}
	out.PlanDigest, _ = Digest(p)
	if r.Leaser == nil {
		return out, fmt.Errorf("coordination requires an exclusive local target lease")
	}
	parentProvider := r.Providers[p.Parent.Repository.Key()]
	if parentProvider == nil {
		return out, fmt.Errorf("parent provider is not authorized")
	}
	for _, child := range p.Children {
		if r.Providers[child.Repository.Key()] == nil {
			return out, fmt.Errorf("child provider is not authorized")
		}
	}
	bindings, err := r.evidenceBindings(p, evidence, out.PlanDigest, true)
	if err != nil {
		return out, err
	}
	if evidence != nil {
		out.EvidenceDigest, _ = Digest(evidence)
	}
	// Lock every plan for this parent, not only this digest. CanonicalKey also
	// includes host identity, unlike a repository-local issue-number lease.
	release, err := r.Leaser.Acquire(ctx, p.Parent.Repository.Ref(), p.Parent.Key())
	if err != nil {
		return out, err
	}
	defer func() { resultErr = errors.Join(resultErr, release()) }()
	pin := "<!-- goobers-coordination:plan:" + out.PlanDigest + " -->"
	if err := r.prepareParent(ctx, p, pin); err != nil {
		return out, err
	}
	defer func() {
		if resultErr != nil {
			out.State, out.Reason = "error", "Coordination failed; inspect the local operator result."
			out.IntegrationArtifactSHA256 = ""
			resultErr = errors.Join(resultErr, r.writeTracking(ctx, p, out))
		}
	}()
	items, err := r.publishBatch(ctx, p, out.PlanDigest)
	if err != nil {
		return out, err
	}
	out.Children, err = r.observeBatch(ctx, p, items, bindings)
	if err != nil {
		return out, err
	}
	if err := r.releaseBatch(ctx, p, items, out.Children); err != nil {
		return out, err
	}
	r.resultState(p, evidence, &out)
	return out, r.writeTracking(ctx, p, out)
}

func (r Reconciler) prepareParent(ctx context.Context, p Plan, pin string) error {
	parentProvider := r.Providers[p.Parent.Repository.Key()]
	parent, err := parentProvider.GetWorkItem(ctx, p.Parent.Repository.Ref(), p.Parent.ID)
	if err != nil {
		return err
	}
	if parent.StateReason == "not_planned" {
		return fmt.Errorf("parent has been cancelled")
	}
	count := strings.Count(parent.Body, "<!-- goobers-coordination:plan:")
	if count > 1 || count == 1 && !strings.Contains(parent.Body, pin) {
		return fmt.Errorf("parent is pinned to a different or ambiguous plan; do not replace an active plan")
	}
	body := parent.Body
	if !strings.Contains(body, pin) {
		if parent.State != "open" {
			return fmt.Errorf("cannot coordinate a closed parent")
		}
		body += "\n\n" + pin
	}
	if body != parent.Body || !parent.HasLabel(providers.LabelCoordinationWait) || parent.HasLabel(providers.LabelReady) {
		_, err = checkedUpdate(ctx, parentProvider, providers.UpdateWorkItemRequest{
			Repository: p.Parent.Repository.Ref(), ID: parent.ID, ExpectedRevision: parent.Revision, Body: &body,
			AddLabels: []string{providers.LabelCoordinationWait}, RemoveLabels: []string{providers.LabelReady},
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (r Reconciler) publishBatch(ctx context.Context, p Plan, digest string) (map[string]providers.WorkItem, error) {
	items := map[string]providers.WorkItem{}
	for _, child := range p.Children {
		item, err := r.publish(ctx, p, child, digest)
		if err != nil {
			return nil, fmt.Errorf("publish %s: %w", child.Key(), err)
		}
		if child.Repository.Key() == p.Parent.Repository.Key() && item.ID == p.Parent.ID {
			return nil, fmt.Errorf("parent issue cannot also be a coordinated child")
		}
		items[child.Key()] = item
	}
	for _, child := range p.Children {
		item := items[child.Key()]
		if !numberPattern.MatchString(item.ID) || item.Title != child.Title {
			return nil, fmt.Errorf("provider returned an invalid child identity or changed its reviewed title")
		}
		if child.Assignee != "" && item.Assignee != child.Assignee {
			return nil, fmt.Errorf("child %s no longer matches its reviewed assignee", child.Key())
		}
		base := child.Body + "\n\n" + childMarker(p, child, digest)
		body := base
		for _, dep := range child.DependsOn {
			fmtRef := dep.Repository.Owner + "/" + dep.Repository.Name + "#" + items[dep.Key()].ID
			body += "\n\nDepends on " + fmtRef + " (" + dep.ID + ")."
		}
		if item.Body != base && item.Body != body {
			return nil, fmt.Errorf("child %s has unreviewed publication content", child.Key())
		}
		if item.Body != body {
			updated, err := checkedUpdate(ctx, r.Providers[child.Repository.Key()], providers.UpdateWorkItemRequest{
				Repository: child.Repository.Ref(), ID: item.ID, ExpectedRevision: item.Revision, Body: &body,
			})
			if err != nil {
				return nil, err
			}
			items[child.Key()] = updated
		}
	}
	return items, nil
}

func (r Reconciler) observeBatch(ctx context.Context, p Plan, items map[string]providers.WorkItem, bindings map[string]Binding) ([]ChildResult, error) {
	out := make([]ChildResult, 0, len(p.Children))
	// Observe the complete batch before releasing anything. Provider failure
	// cannot silently satisfy a dependency or leave a successful result.
	for _, child := range p.Children {
		observation, err := r.observe(ctx, child, items[child.Key()], bindings[child.Key()])
		if err != nil {
			observation.State, observation.Reason = "blocked", "provider observation failed"
		}
		out = append(out, observation)
		if err != nil {
			return out, fmt.Errorf("observe %s: %w", child.Key(), err)
		}
	}
	return out, nil
}

func dependencySatisfaction(p Plan, byKey map[string]*ChildResult) map[string]bool {
	plans := map[string]Child{}
	for _, child := range p.Children {
		plans[child.Key()] = child
	}
	var satisfied func(string) bool
	evaluated, satisfaction := map[string]bool{}, map[string]bool{}
	satisfied = func(key string) bool {
		if evaluated[key] {
			return satisfaction[key]
		}
		evaluated[key] = true
		if byKey[key].State != "complete" {
			return false
		}
		for _, dep := range plans[key].DependsOn {
			if !satisfied(dep.Key()) {
				return false
			}
		}
		satisfaction[key] = true
		return true
	}
	for _, child := range p.Children {
		satisfied(child.Key())
	}
	return satisfaction
}

func (r Reconciler) releaseBatch(ctx context.Context, p Plan, items map[string]providers.WorkItem, children []ChildResult) error {
	byKey := map[string]*ChildResult{}
	for i := range children {
		byKey[children[i].Key()] = &children[i]
	}
	satisfied := dependencySatisfaction(p, byKey)
	busy := map[string]string{}
	for _, child := range p.Children {
		if occupiesSlot(*byKey[child.Key()], items[child.Key()]) {
			if busy[child.Repository.Key()] == "" {
				busy[child.Repository.Key()] = child.Key()
			}
		}
	}
	for _, child := range p.Children {
		observed := byKey[child.Key()]
		item := items[child.Key()]
		eligible := observed.State == "pending"
		approved := observed.State != "failed" && observed.State != "blocked"
		for _, dep := range child.DependsOn {
			if !satisfied[dep.Key()] {
				eligible, approved = false, false
				if observed.State != "failed" {
					observed.State, observed.Reason = "blocked", "dependency "+dep.Key()+" is "+byKey[dep.Key()].State
				}
			}
		}
		if eligible && busy[child.Repository.Key()] != "" && busy[child.Repository.Key()] != child.Key() {
			eligible, approved = false, false
			observed.State, observed.Reason = "blocked", "another child owns this repository's implementation slot"
		}
		if eligible {
			busy[child.Repository.Key()] = child.Key()
		}
		if err := r.setEligibility(ctx, child, item, eligible, approved); err != nil {
			return err
		}
		if eligible {
			observed.State = "ready"
		}
	}
	return nil
}

func occupiesSlot(child ChildResult, item providers.WorkItem) bool {
	switch child.State {
	case "in-review", "in-progress":
		return true
	case "pending":
		return item.HasLabel(providers.LabelReady)
	default:
		return false
	}
}

func (r Reconciler) resultState(p Plan, evidence *Evidence, out *Result) {
	out.State = "waiting"
	for _, child := range out.Children {
		if child.State == "failed" || child.State == "blocked" {
			out.State = "blocked"
			break
		}
	}
	allComplete := true
	for _, child := range out.Children {
		allComplete = allComplete && child.State == "complete"
	}
	if allComplete {
		if err := r.integration(p, evidence, out.Children); err != nil {
			out.State, out.Reason = "integration-required", err.Error()
		} else {
			out.State = "complete"
			out.IntegrationArtifactSHA256 = r.ArtifactDigest
		}
	}
}

func (r Reconciler) writeTracking(ctx context.Context, p Plan, out Result) error {
	parentProvider := r.Providers[p.Parent.Repository.Key()]
	parent, err := parentProvider.GetWorkItem(ctx, p.Parent.Repository.Ref(), p.Parent.ID)
	if err != nil {
		return err
	}
	pin := "<!-- goobers-coordination:plan:" + out.PlanDigest + " -->"
	if strings.Count(parent.Body, "<!-- goobers-coordination:plan:") != 1 || !strings.Contains(parent.Body, pin) {
		return fmt.Errorf("parent plan pin changed during reconciliation")
	}
	body, err := trackingBody(parent.Body, p, out)
	if err != nil {
		return err
	}
	state := ""
	if out.State == "complete" {
		state = "closed"
	} else if parent.State == "closed" {
		state = "open"
	}
	if body != parent.Body || state != "" && state != parent.State {
		_, err = checkedUpdate(ctx, parentProvider, providers.UpdateWorkItemRequest{
			Repository: p.Parent.Repository.Ref(), ID: parent.ID, ExpectedRevision: parent.Revision, Body: &body, State: state,
		})
	}
	return err
}

// ValidateEvidence performs offline binding validation before any credentials
// are resolved. Approval is optional only for the operator's --check preview.
func ValidateEvidence(p Plan, e *Evidence, a Authority, requireApproval bool) error {
	digest, _ := Digest(p)
	_, err := (Reconciler{Authority: a}).evidenceBindings(p, e, digest, requireApproval)
	return err
}

func (r Reconciler) evidenceBindings(p Plan, e *Evidence, digest string, requireApproval bool) (map[string]Binding, error) {
	bindings := map[string]Binding{}
	if e == nil {
		return bindings, nil
	}
	evidenceDigest, err := Digest(e)
	if err != nil {
		return nil, err
	}
	if e.PlanDigest != digest || requireApproval && r.Authority.ApprovedEvidence[p.ID] != evidenceDigest {
		return nil, fmt.Errorf("evidence is not approved for this exact plan")
	}
	known := map[string]bool{}
	pulls := map[string]bool{}
	for _, child := range p.Children {
		known[child.Key()] = true
	}
	for _, binding := range e.Children {
		if !known[binding.Key()] || !numberPattern.MatchString(binding.Issue) {
			return nil, fmt.Errorf("evidence has unknown child or invalid issue")
		}
		if _, exists := bindings[binding.Key()]; exists {
			return nil, fmt.Errorf("duplicate evidence binding")
		}
		if binding.PR != "" && (!numberPattern.MatchString(binding.PR) || !shaPattern.MatchString(binding.HeadSHA)) {
			return nil, fmt.Errorf("PR binding requires a positive number and exact head SHA")
		}
		if binding.PR != "" {
			key := binding.Repository.Key() + "#" + binding.PR
			if pulls[key] {
				return nil, fmt.Errorf("one pull request cannot implement multiple child issues")
			}
			pulls[key] = true
		}
		if binding.MergeSHA != "" && !shaPattern.MatchString(binding.MergeSHA) {
			return nil, fmt.Errorf("invalid merge SHA")
		}
		if binding.ReleaseSHA != "" && !shaPattern.MatchString(binding.ReleaseSHA) {
			return nil, fmt.Errorf("invalid release SHA")
		}
		bindings[binding.Key()] = binding
	}
	return bindings, nil
}

func childMarker(p Plan, c Child, digest string) string {
	key, _ := Digest([]string{p.Parent.Key(), p.ID, c.Key()})
	return "<!-- goobers-coordination:child:" + key + ":" + digest + " -->"
}

func (r Reconciler) publish(ctx context.Context, p Plan, c Child, digest string) (providers.WorkItem, error) {
	provider := r.Providers[c.Repository.Key()]
	marker := childMarker(p, c, digest)
	body := c.Body + "\n\n" + marker
	items, err := provider.FindWorkItemsByMarker(ctx, c.Repository.Ref(), marker)
	if err != nil {
		return providers.WorkItem{}, err
	}
	if len(items) > 1 {
		return providers.WorkItem{}, fmt.Errorf("duplicate durable child marker; human reconciliation required")
	}
	if len(items) == 1 && (NodeRef{Repository: c.Repository, ID: items[0].ID}).Key() == p.Parent.Key() {
		return providers.WorkItem{}, fmt.Errorf("parent issue cannot also be a coordinated child")
	}
	if len(items) == 0 {
		parentProvider := r.Providers[p.Parent.Repository.Key()]
		parent, err := parentProvider.GetWorkItem(ctx, p.Parent.Repository.Ref(), p.Parent.ID)
		if err != nil {
			return providers.WorkItem{}, err
		}
		intentKey, _ := Digest([]string{p.Parent.Key(), p.ID, c.Key()})
		intent := "<!-- goobers-coordination:intent:" + intentKey + " -->"
		if strings.Contains(parent.Body, intent) {
			return providers.WorkItem{}, fmt.Errorf("prior create intent has no discoverable issue; restore the original marker or manually reconcile publication from --check; refusing a duplicate")
		}
		parentBody := parent.Body + "\n\n" + intent
		if _, err := checkedUpdate(ctx, parentProvider, providers.UpdateWorkItemRequest{
			Repository: p.Parent.Repository.Ref(), ID: parent.ID, ExpectedRevision: parent.Revision, Body: &parentBody,
		}); err != nil {
			return providers.WorkItem{}, err
		}
		item, err := provider.CreateWorkItem(ctx, providers.CreateWorkItemRequest{
			Repository: c.Repository.Ref(), Title: c.Title, Body: body,
			Labels:   []string{providers.LabelCoordinationWait},
			Assignee: c.Assignee,
		})
		if err == nil && !item.HasLabel(providers.LabelCoordinationWait) {
			return item, fmt.Errorf("provider did not apply coordination wait label to the new child")
		}
		return item, err
	}
	item, err := provider.GetWorkItem(ctx, c.Repository.Ref(), items[0].ID)
	if err != nil {
		return item, err
	}
	if item.Title != c.Title || item.Body != body && !strings.HasPrefix(item.Body, body+"\n\nDepends on ") {
		return item, fmt.Errorf("published child content changed; refusing to approve unreviewed text")
	}
	return item, nil
}

func observeIssue(c Child, item providers.WorkItem) (ChildResult, bool) {
	out := ChildResult{NodeRef: c.NodeRef, Issue: item.ID, State: "pending", IssueClosed: item.State == "closed", ReleaseTag: c.ReleaseTag}
	if item.StateReason == "not_planned" {
		out.State, out.Reason = "failed", "issue cancelled/not planned"
		return out, true
	}
	if item.HasLabel(providers.LabelNeedsHuman) {
		out.State, out.Reason = "blocked", "implementation requires human attention"
		return out, true
	}
	if c.Completion == "closed" {
		if out.IssueClosed && item.StateReason == "completed" {
			out.State = "complete"
		} else if out.IssueClosed {
			out.State, out.Reason = "blocked", "closed task lacks an explicit completed reason"
		}
		return out, true
	}
	return out, false
}

func (r Reconciler) observe(ctx context.Context, c Child, item providers.WorkItem, binding Binding) (ChildResult, error) {
	out, terminal := observeIssue(c, item)
	if terminal {
		return out, nil
	}
	if binding.PR == "" {
		if out.IssueClosed {
			out.State, out.Reason = "blocked", "issue closed without approved PR evidence"
		} else if item.Status == providers.WorkItemStatusInReview {
			out.State, out.Reason = "in-review", "owner reports in-review; approved PR binding required"
		} else if item.HasLabel(providers.LabelClaimed) || item.Status == providers.WorkItemStatusInProgress || item.Status == providers.WorkItemStatusClaimed {
			out.State = "in-progress"
		}
		return out, nil
	}
	if binding.Issue != item.ID {
		return out, fmt.Errorf("approved PR binding names a different child issue")
	}
	pr, err := r.Providers[c.Repository.Key()].GetPullRequest(ctx, c.Repository.Ref(), binding.PR)
	if err != nil {
		return out, err
	}
	out.PR, out.PRMerged, out.HeadSHA, out.MergeSHA = binding.PR, pr.Merged, pr.HeadSHA, pr.MergeSHA
	if pr.HeadSHA != binding.HeadSHA {
		out.State, out.Reason = "blocked", "PR head changed; evidence must be reviewed again"
		return out, nil
	}
	if !pr.Merged {
		out.State = "in-review"
		if pr.State == "closed" {
			out.State, out.Reason = "failed", "PR abandoned without merge"
		}
		return out, nil
	}
	if binding.MergeSHA == "" || pr.MergeSHA != binding.MergeSHA {
		out.State, out.Reason = "blocked", "merge SHA lacks exact approved evidence"
		return out, nil
	}
	out.State = "complete"
	if c.Completion == "release" {
		release, err := r.Providers[c.Repository.Key()].GetCoordinationRelease(ctx, c.Repository.Ref(), c.ReleaseTag, pr.MergeSHA)
		if err != nil {
			return out, err
		}
		out.ReleaseSHA = release.SHA
		if !release.Published || !release.IncludesCommit || release.Tag != c.ReleaseTag || binding.ReleaseSHA == "" || release.SHA != binding.ReleaseSHA {
			out.State, out.Reason = "blocked", "required published release/version lacks exact approved commit evidence"
		}
	}
	return out, nil
}

func (r Reconciler) setEligibility(ctx context.Context, c Child, item providers.WorkItem, eligible, approved bool) error {
	add, remove := []string{}, []string{}
	for _, label := range []string{providers.LabelApproved, providers.LabelReady, providers.LabelCoordinationWait} {
		wanted := eligible
		if label == providers.LabelApproved {
			wanted = approved
		}
		if label == providers.LabelCoordinationWait {
			wanted = !approved
		}
		if wanted && !item.HasLabel(label) {
			add = append(add, label)
		}
		if !wanted && item.HasLabel(label) {
			remove = append(remove, label)
		}
	}
	if len(add)+len(remove) == 0 {
		return nil
	}
	_, err := checkedUpdate(ctx, r.Providers[c.Repository.Key()], providers.UpdateWorkItemRequest{
		Repository: c.Repository.Ref(), ID: item.ID, ExpectedRevision: item.Revision, AddLabels: add, RemoveLabels: remove,
	})
	return err
}

func checkedUpdate(ctx context.Context, provider Provider, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	item, err := provider.UpdateWorkItem(ctx, req)
	if err != nil {
		return item, err
	}
	if item.ID != req.ID || item.Revision == "" {
		return item, fmt.Errorf("provider update returned an invalid work item identity/revision")
	}
	if req.Body != nil && item.Body != *req.Body {
		return item, fmt.Errorf("provider did not persist the reviewed coordination body")
	}
	if req.State != "" && item.State != req.State {
		return item, fmt.Errorf("provider did not persist the requested parent state")
	}
	for _, label := range req.AddLabels {
		if !item.HasLabel(label) {
			return item, fmt.Errorf("provider did not apply required label %s; check repository label provisioning", label)
		}
	}
	for _, label := range req.RemoveLabels {
		if item.HasLabel(label) {
			return item, fmt.Errorf("provider did not withdraw eligibility label %s", label)
		}
	}
	return item, nil
}

func (r Reconciler) integration(p Plan, e *Evidence, children []ChildResult) error {
	if e == nil || e.Integration == nil {
		return fmt.Errorf("approved integration evidence is required")
	}
	i := e.Integration
	if !i.Passed || i.Command != p.IntegrationCommand || !digestPattern.MatchString(i.ArtifactSHA256) || i.ArtifactSHA256 != r.ArtifactDigest {
		return fmt.Errorf("integration requires passing reviewed command and matching local artifact SHA-256")
	}
	pins := map[string]Binding{}
	for _, pin := range i.Children {
		if _, exists := pins[pin.Key()]; exists {
			return fmt.Errorf("duplicate integration pin")
		}
		pins[pin.Key()] = pin
	}
	if len(pins) != len(children) {
		return fmt.Errorf("integration must pin every child exactly once")
	}
	for _, child := range children {
		pin, exists := pins[child.Key()]
		if !exists || pin.Issue != child.Issue || pin.PR != child.PR || pin.HeadSHA != child.HeadSHA || pin.MergeSHA != child.MergeSHA || pin.ReleaseSHA != child.ReleaseSHA {
			return fmt.Errorf("integration evidence is stale for %s", child.Key())
		}
	}
	return nil
}

func trackingBody(body string, p Plan, out Result) (string, error) {
	const start, end = "<!-- goobers-coordination:tracking:start -->", "<!-- goobers-coordination:tracking:end -->"
	i, j := strings.Index(body, start), strings.Index(body, end)
	if (i < 0) != (j < 0) || i >= 0 && j < i || strings.Count(body, start) > 1 || strings.Count(body, end) > 1 {
		return "", fmt.Errorf("invalid parent tracking markers")
	}
	var section strings.Builder
	fmt.Fprintf(&section, "%s\n%s\n\nCoordination: **%s**\n", start, p.Summary, out.State)
	fmt.Fprintf(&section, "\nPlan SHA-256: `%s`.\n", out.PlanDigest)
	if out.EvidenceDigest != "" {
		fmt.Fprintf(&section, "\nEvidence SHA-256: `%s`.\n", out.EvidenceDigest)
	}
	if out.IntegrationArtifactSHA256 != "" {
		fmt.Fprintf(&section, "\nIntegration artifact SHA-256: `%s`.\n", out.IntegrationArtifactSHA256)
	}
	for _, child := range out.Children {
		fmt.Fprintf(&section, "\n- %s/%s#%s (%s): %s", child.Repository.Owner, child.Repository.Name, child.Issue, child.ID, child.State)
		if child.PR != "" {
			fmt.Fprintf(&section, "; PR %s/%s#%s", child.Repository.Owner, child.Repository.Name, child.PR)
		}
		if child.MergeSHA != "" {
			fmt.Fprintf(&section, "; merge `%s`", child.MergeSHA)
		}
		if child.ReleaseSHA != "" {
			fmt.Fprintf(&section, "; release `%s` at `%s`", child.ReleaseTag, child.ReleaseSHA)
		}
	}
	fmt.Fprintf(&section, "\n%s", end)
	if i < 0 {
		return body + "\n\n" + section.String(), nil
	}
	return body[:i] + section.String() + body[j+len(end):], nil
}
