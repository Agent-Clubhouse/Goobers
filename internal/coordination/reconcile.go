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
	PlanID     string        `json:"planId"`
	PlanDigest string        `json:"planDigest"`
	State      string        `json:"state"`
	Reason     string        `json:"reason,omitempty"`
	Children   []ChildResult `json:"children"`
}

type Publication struct {
	NodeRef
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Publications is the exact initial public content, also usable for manual
// recovery when a durable create intent has an ambiguous provider outcome.
func Publications(p Plan) []Publication {
	digest, _ := Digest(p)
	out := make([]Publication, 0, len(p.Children))
	for _, child := range p.Children {
		out = append(out, Publication{NodeRef: child.NodeRef, Title: child.Title, Body: child.Body + "\n\n" + childMarker(p, child, digest)})
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
	bindings, err := r.approvedBindings(p, evidence, out.PlanDigest)
	if err != nil {
		return out, err
	}
	// Lock every plan for this parent, not only this digest. CanonicalKey also
	// includes host identity, unlike a repository-local issue-number lease.
	release, err := r.Leaser.Acquire(ctx, p.Parent.Repository.Ref(), p.Parent.Key())
	if err != nil {
		return out, err
	}
	defer func() { resultErr = errors.Join(resultErr, release()) }()
	parent, err := parentProvider.GetWorkItem(ctx, p.Parent.Repository.Ref(), p.Parent.ID)
	if err != nil {
		return out, err
	}
	if parent.StateReason == "not_planned" {
		return out, fmt.Errorf("parent has been cancelled")
	}
	pin := "<!-- goobers-coordination:plan:" + out.PlanDigest + " -->"
	if strings.Contains(parent.Body, "<!-- goobers-coordination:plan:") && !strings.Contains(parent.Body, pin) {
		return out, fmt.Errorf("parent is pinned to a different plan; do not replace an active plan")
	}
	if !strings.Contains(parent.Body, pin) {
		if parent.State != "open" {
			return out, fmt.Errorf("cannot coordinate a closed parent")
		}
		body := parent.Body + "\n\n" + pin
		parent, err = parentProvider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: p.Parent.Repository.Ref(), ID: parent.ID, ExpectedRevision: parent.Revision, Body: &body,
		})
		if err != nil {
			return out, err
		}
	}
	defer func() {
		if resultErr == nil {
			return
		}
		// Provider errors can contain private context or credentials. Publish
		// only this fixed failure summary; the CLI scrubs the detailed error.
		out.State, out.Reason = "error", "Coordination failed; inspect the local operator result."
		current, err := parentProvider.GetWorkItem(ctx, p.Parent.Repository.Ref(), p.Parent.ID)
		if err != nil {
			resultErr = errors.Join(resultErr, err)
			return
		}
		if !strings.Contains(current.Body, pin) {
			return
		}
		body, err := trackingBody(current.Body, p, out)
		if err != nil {
			resultErr = errors.Join(resultErr, err)
			return
		}
		_, err = parentProvider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: p.Parent.Repository.Ref(), ID: current.ID, ExpectedRevision: current.Revision, Body: &body, State: "open",
		})
		resultErr = errors.Join(resultErr, err)
	}()
	items := map[string]providers.WorkItem{}
	for _, child := range p.Children {
		item, err := r.publish(ctx, p, child, out.PlanDigest)
		if err != nil {
			return out, fmt.Errorf("publish %s: %w", child.Key(), err)
		}
		items[child.Key()] = item
	}
	for _, child := range p.Children {
		item := items[child.Key()]
		base := child.Body + "\n\n" + childMarker(p, child, out.PlanDigest)
		body := base
		for _, dep := range child.DependsOn {
			fmtRef := dep.Repository.Owner + "/" + dep.Repository.Name + "#" + items[dep.Key()].ID
			body += "\n\nDepends on " + fmtRef + " (" + dep.ID + ")."
		}
		if item.Body != base && item.Body != body {
			return out, fmt.Errorf("child %s has unreviewed publication content", child.Key())
		}
		if item.Body != body {
			item, err = r.Providers[child.Repository.Key()].UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
				Repository: child.Repository.Ref(), ID: item.ID, ExpectedRevision: item.Revision, Body: &body,
			})
			if err != nil {
				return out, err
			}
			items[child.Key()] = item
		}
	}
	// Observe the complete batch before releasing anything. Provider failure
	// cannot silently satisfy a dependency or leave a successful result.
	for _, child := range p.Children {
		observation, err := r.observe(ctx, child, items[child.Key()], bindings[child.Key()])
		if err != nil {
			observation.State, observation.Reason = "blocked", "provider observation failed"
		}
		out.Children = append(out.Children, observation)
		if err != nil {
			return out, fmt.Errorf("observe %s: %w", child.Key(), err)
		}
	}
	byKey := map[string]*ChildResult{}
	for i := range out.Children {
		byKey[out.Children[i].Key()] = &out.Children[i]
	}
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
	busy := map[string]string{}
	for _, child := range p.Children {
		if byKey[child.Key()].State == "in-review" || byKey[child.Key()].State == "pending" && items[child.Key()].HasLabel(providers.LabelReady) {
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
			if !satisfied(dep.Key()) {
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
			return out, err
		}
		if eligible {
			observed.State = "ready"
		}
	}
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
		}
	}
	parent, err = parentProvider.GetWorkItem(ctx, p.Parent.Repository.Ref(), p.Parent.ID)
	if err != nil {
		return out, err
	}
	if !strings.Contains(parent.Body, pin) {
		return out, fmt.Errorf("parent plan pin changed during reconciliation")
	}
	body, err := trackingBody(parent.Body, p, out)
	if err != nil {
		return out, err
	}
	state := ""
	if out.State == "complete" {
		state = "closed"
	} else if parent.State == "closed" {
		state = "open"
	}
	if body != parent.Body || state != "" && state != parent.State {
		_, err = parentProvider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: p.Parent.Repository.Ref(), ID: parent.ID, ExpectedRevision: parent.Revision, Body: &body, State: state,
		})
	}
	return out, err
}

func (r Reconciler) approvedBindings(p Plan, e *Evidence, digest string) (map[string]Binding, error) {
	bindings := map[string]Binding{}
	if e == nil {
		return bindings, nil
	}
	evidenceDigest, err := Digest(e)
	if err != nil {
		return nil, err
	}
	if e.PlanDigest != digest || r.Authority.ApprovedEvidence[p.ID] != evidenceDigest {
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
		if _, err := parentProvider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: p.Parent.Repository.Ref(), ID: parent.ID, ExpectedRevision: parent.Revision, Body: &parentBody,
		}); err != nil {
			return providers.WorkItem{}, err
		}
		return provider.CreateWorkItem(ctx, providers.CreateWorkItemRequest{
			Repository: c.Repository.Ref(), Title: c.Title, Body: body,
			Labels: []string{providers.LabelNeedsHuman},
		})
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

func (r Reconciler) observe(ctx context.Context, c Child, item providers.WorkItem, binding Binding) (ChildResult, error) {
	out := ChildResult{NodeRef: c.NodeRef, Issue: item.ID, State: "pending", IssueClosed: item.State == "closed", ReleaseTag: c.ReleaseTag}
	if item.StateReason == "not_planned" {
		out.State, out.Reason = "failed", "issue cancelled/not planned"
		return out, nil
	}
	if c.Completion == "closed" {
		if out.IssueClosed && item.StateReason == "completed" {
			out.State = "complete"
		}
		return out, nil
	}
	if binding.PR == "" {
		if out.IssueClosed {
			out.State, out.Reason = "blocked", "issue closed without approved PR evidence"
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
	for _, label := range []string{providers.LabelApproved, providers.LabelReady} {
		wanted := eligible
		if label == providers.LabelApproved {
			wanted = approved
		}
		if wanted && !item.HasLabel(label) {
			add = append(add, label)
		}
		if !wanted && item.HasLabel(label) {
			remove = append(remove, label)
		}
	}
	if eligible && item.HasLabel(providers.LabelNeedsHuman) {
		remove = append(remove, providers.LabelNeedsHuman)
	}
	if len(add)+len(remove) == 0 {
		return nil
	}
	_, err := r.Providers[c.Repository.Key()].UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository: c.Repository.Ref(), ID: item.ID, ExpectedRevision: item.Revision, AddLabels: add, RemoveLabels: remove,
	})
	return err
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
	if (i < 0) != (j < 0) || i >= 0 && j < i {
		return "", fmt.Errorf("invalid parent tracking markers")
	}
	var section strings.Builder
	fmt.Fprintf(&section, "%s\n%s\n\nCoordination: **%s**\n", start, p.Summary, out.State)
	for _, child := range out.Children {
		fmt.Fprintf(&section, "\n- %s/%s#%s (%s): %s", child.Repository.Owner, child.Repository.Name, child.Issue, child.ID, child.State)
		if child.PR != "" {
			fmt.Fprintf(&section, "; PR %s/%s#%s", child.Repository.Owner, child.Repository.Name, child.PR)
		}
	}
	fmt.Fprintf(&section, "\n%s", end)
	if i < 0 {
		return body + "\n\n" + section.String(), nil
	}
	return body[:i] + section.String() + body[j+len(end):], nil
}
