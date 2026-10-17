package nomination

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/flake"
	"github.com/goobers/goobers/providers"
)

var publisherNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

type fakeProvider struct {
	open, closed, flakes []providers.WorkItem
	comments             map[string][]providers.Comment
	byID                 map[string]providers.WorkItem

	created   []providers.CreateWorkItemRequest
	updated   []providers.UpdateWorkItemRequest
	commented map[string][]string

	listErr, createErr, getErr, updateErr, commentsErr, commentErr error
}

func (f *fakeProvider) ListWorkItems(_ context.Context, req providers.ListWorkItemsRequest) ([]providers.WorkItem, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	for _, l := range req.Labels {
		if l == FlakeLabel {
			return f.flakes, nil
		}
	}
	if req.State == "closed" {
		return f.closed, nil
	}
	return f.open, nil
}

func (f *fakeProvider) GetWorkItem(_ context.Context, _ providers.RepositoryRef, id string) (providers.WorkItem, error) {
	if f.getErr != nil {
		return providers.WorkItem{}, f.getErr
	}
	return f.byID[id], nil
}

func (f *fakeProvider) CreateWorkItem(_ context.Context, req providers.CreateWorkItemRequest) (providers.WorkItem, error) {
	if f.createErr != nil {
		return providers.WorkItem{}, f.createErr
	}
	f.created = append(f.created, req)
	return providers.WorkItem{ID: "100", Revision: "r1", Title: req.Title, Body: req.Body, Labels: req.Labels}, nil
}

func (f *fakeProvider) UpdateWorkItem(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	if f.updateErr != nil {
		return providers.WorkItem{}, f.updateErr
	}
	f.updated = append(f.updated, req)
	item := f.byID[req.ID]
	if item.ID == "" {
		item = providers.WorkItem{ID: req.ID, Labels: []string{"base"}}
		for _, c := range f.created {
			if req.ID == "100" {
				item.Labels = c.Labels
			}
		}
	}
	item.Labels = append(append([]string(nil), item.Labels...), req.AddLabels...)
	return item, nil
}

func (f *fakeProvider) ListComments(_ context.Context, _ providers.RepositoryRef, id string) ([]providers.Comment, error) {
	if f.commentsErr != nil {
		return nil, f.commentsErr
	}
	return f.comments[id], nil
}

func (f *fakeProvider) CreateWorkItemComment(_ context.Context, _ providers.RepositoryRef, id, body string) (providers.Comment, error) {
	if f.commentErr != nil {
		return providers.Comment{}, f.commentErr
	}
	if f.commented == nil {
		f.commented = map[string][]string{}
	}
	f.commented[id] = append(f.commented[id], body)
	return providers.Comment{}, nil
}

type fakeApprover struct {
	calls []providers.UpdateWorkItemRequest
	err   error
}

func (a *fakeApprover) UpdateWorkItem(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	if a.err != nil {
		return providers.WorkItem{}, a.err
	}
	a.calls = append(a.calls, req)
	return providers.WorkItem{ID: req.ID, Labels: append([]string{"base"}, req.AddLabels...)}, nil
}

const (
	pubVetRule = "result of (*os.File).Close call not used"
	pubVetPath = "internal/worktree/manager.go"
)

func pubPolicy() Policy {
	return Policy{
		BacklogLabel: "goobers:backlog", PartitionLabel: "goobers:cloud", NominatedLabel: "goobers:nominated",
		MaxPerRun: 5, DedupeWindow: 24 * time.Hour, AutoApprove: true,
	}
}

func pubFindings() *Findings {
	return ParseSignals([]byte("=== go vet (exit 1) ===\n" + pubVetPath + ":88:2: " + pubVetRule + "\n"))
}

func pubFindingNomination() Nomination {
	return Nomination{
		Key: "vet-close", DedupeKey: "vet:close", Title: "go vet: close", Body: "go vet reports an unchecked Close result in internal/worktree/manager.go.",
		Labels: []string{"type:bug"}, RiskClass: RiskLow, RiskReason: "vet",
		Evidence: []Evidence{{Kind: EvidenceFinding, Tool: ToolVet, Path: pubVetPath, Line: 88, Rule: pubVetRule}},
	}
}

func pubArtifact(ns ...Nomination) Artifact {
	return Artifact{Schema: SchemaV1, RunID: testRunID, Producer: Producer{Stage: "triage", Attempt: 1}, Nominations: ns}
}

func newPublisher(p *fakeProvider, a Approver) Publisher {
	return Publisher{
		Provider: p, Approver: a, Findings: pubFindings(),
		Repo: providers.RepositoryRef{Name: "goobers"}, RunID: testRunID, Policy: pubPolicy(),
		Now: func() time.Time { return publisherNow },
	}
}

func joined(s []string) string { return strings.Join(s, "\n") }

func TestPublishApprovesConfirmedFinding(t *testing.T) {
	prov := &fakeProvider{}
	appr := &fakeApprover{}
	res, err := newPublisher(prov, appr).Publish(context.Background(), pubArtifact(pubFindingNomination()))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Filed) != 1 || !res.Filed[0].Approved || res.Approved() != 1 {
		t.Fatalf("filed = %+v", res.Filed)
	}
	if len(appr.calls) != 1 || appr.calls[0].AddLabels[0] != providers.LabelApproved {
		t.Fatalf("approver calls = %+v", appr.calls)
	}
	for _, l := range prov.created[0].Labels {
		if l == providers.LabelApproved {
			t.Fatal("write credential must never apply goobers:approved")
		}
	}
}

func TestPublishFailsClosedOnApproval(t *testing.T) {
	human := pubFindingNomination()
	human.RiskClass = RiskHuman
	human.RiskReason = "needs a person"
	fabricated := pubFindingNomination()
	fabricated.Evidence[0].Line = 999

	for name, tc := range map[string]struct {
		mutate func(*Publisher)
		nom    Nomination
		want   string
	}{
		"opt-in off":       {func(p *Publisher) { p.Policy.AutoApprove = false }, pubFindingNomination(), "autoApprove is never on"},
		"no approver":      {func(p *Publisher) { p.Approver = nil }, pubFindingNomination(), "approve credential did not resolve"},
		"findings missing": {func(p *Publisher) { p.Findings = nil; p.FindingsUnavailable = "pod cannot read journal" }, pubFindingNomination(), "pod cannot read journal"},
		"findings default": {func(p *Publisher) { p.Findings = nil }, pubFindingNomination(), "not readable from this stage"},
		"fabricated":       {func(*Publisher) {}, fabricated, "does not contain"},
		"high risk":        {func(*Publisher) {}, human, "not low"},
	} {
		t.Run(name, func(t *testing.T) {
			prov := &fakeProvider{}
			appr := &fakeApprover{}
			pub := newPublisher(prov, appr)
			tc.mutate(&pub)
			if pub.Approver != nil {
				pub.Approver = appr
			}
			res, err := pub.Publish(context.Background(), pubArtifact(tc.nom))
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Filed) != 1 || res.Filed[0].Approved || res.Approved() != 0 {
				t.Fatalf("filed = %+v", res.Filed)
			}
			if !strings.Contains(joined(res.Filed[0].ApprovalUnmet), tc.want) {
				t.Fatalf("unmet = %v, want %q", res.Filed[0].ApprovalUnmet, tc.want)
			}
			if len(appr.calls) != 0 {
				t.Fatalf("approver must not be called: %+v", appr.calls)
			}
		})
	}
}

func TestApprovalUnmetBounds(t *testing.T) {
	base := func() Candidate {
		n := pubFindingNomination()
		return Candidate{Nomination: n, Findings: []Finding{findingOf(n.Evidence[0])}}
	}
	pub := newPublisher(&fakeProvider{}, nil)
	priors := func() priorFindings { return priorFindings{claimed: map[string]string{}} }

	if unmet := pub.approvalUnmet(base(), priors()); len(unmet) != 0 {
		t.Fatalf("clean candidate unmet = %v", unmet)
	}
	for name, tc := range map[string]struct {
		mutate func(*Candidate)
		want   string
	}{
		"no type:bug":  {func(c *Candidate) { c.Nomination.Labels = []string{"area:runner"} }, "type:bug"},
		"human review": {func(c *Candidate) { c.Nomination.RequiresHumanReview = true }, "requires human review"},
		"load bearing": {func(c *Candidate) {
			c.Nomination.Evidence = append(c.Nomination.Evidence, Evidence{Kind: EvidenceSource, Path: "api/x.go"})
		}, "load-bearing"},
		"no finding evd": {func(c *Candidate) { c.Findings = nil; c.Nomination.Evidence = nil }, "no evidence names a deterministic tool finding"},
	} {
		c := base()
		tc.mutate(&c)
		if unmet := joined(pub.approvalUnmet(c, priors())); !strings.Contains(unmet, tc.want) {
			t.Errorf("%s: unmet = %q, want %q", name, unmet, tc.want)
		}
	}
}

func TestDuplicateFindingUnmet(t *testing.T) {
	f := findingOf(pubFindingNomination().Evidence[0])
	hash := FindingHash(f)
	cutoff := publisherNow.Add(-24 * time.Hour)
	recent := publisherNow.Add(-time.Hour)
	old := publisherNow.Add(-72 * time.Hour)
	ownKey := KeyHash("vet:close")

	for name, tc := range map[string]struct {
		window  time.Duration
		claimed map[string]string
		prior   *providers.WorkItem
		want    string
	}{
		"none":                   {24 * time.Hour, nil, nil, ""},
		"claimed in this scan":   {24 * time.Hour, map[string]string{hash: "sibling"}, nil, `nomination "sibling"`},
		"open prior":             {24 * time.Hour, nil, &providers.WorkItem{ID: "7", State: "open"}, "open issue #7"},
		"closed inside window":   {24 * time.Hour, nil, &providers.WorkItem{ID: "8", State: "closed", UpdatedAt: &recent}, "closed inside the dedupe window"},
		"closed outside window":  {24 * time.Hour, nil, &providers.WorkItem{ID: "9", State: "closed", UpdatedAt: &old}, ""},
		"closed with no window":  {0, nil, &providers.WorkItem{ID: "10", State: "closed", UpdatedAt: &recent}, ""},
		"own filing is no prior": {24 * time.Hour, nil, &providers.WorkItem{ID: "11", State: "open", Body: FiledMarker(ownKey, testRunID)}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			pub := newPublisher(&fakeProvider{}, nil)
			pub.Policy.DedupeWindow = tc.window
			pf := priorFindings{cutoff: cutoff, byFinding: map[string][]providers.WorkItem{}, artifactKeys: map[string]bool{ownKey: true}, claimed: tc.claimed}
			if pf.claimed == nil {
				pf.claimed = map[string]string{}
			}
			if tc.prior != nil {
				pf.byFinding[hash] = []providers.WorkItem{*tc.prior}
			}
			got := joined(pub.duplicateFindingUnmet([]Finding{f}, pf))
			if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
				t.Fatalf("unmet = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestScanDedupe(t *testing.T) {
	nom := pubFindingNomination()
	hash := KeyHash(nom.DedupeKey)
	recent := publisherNow.Add(-time.Hour)
	old := publisherNow.Add(-72 * time.Hour)
	fp := FindingHash(findingOf(nom.Evidence[0]))

	for name, tc := range map[string]struct {
		prov       fakeProvider
		wantFile   int
		wantSup    string
		wantUnmet  string
		wantOwned  string
		wantOpenID string
	}{
		"open key suppresses": {
			prov: fakeProvider{open: []providers.WorkItem{{ID: "5", State: "open", Body: KeyMarker(hash)}}}, wantSup: "open issue #5", wantOpenID: "5",
		},
		"closed in window suppresses": {
			prov: fakeProvider{closed: []providers.WorkItem{{ID: "6", State: "closed", UpdatedAt: &recent, Body: KeyMarker(hash)}}}, wantSup: "closed inside the dedupe window",
		},
		"closed outside window files": {
			prov: fakeProvider{closed: []providers.WorkItem{{ID: "6", State: "closed", UpdatedAt: &old, Body: KeyMarker(hash)}}}, wantFile: 1,
		},
		"flake owner suppresses": {
			prov: fakeProvider{}, wantSup: "flake-watch already owns",
		},
		"own prior filing is read back": {
			prov: fakeProvider{open: []providers.WorkItem{{ID: "12", State: "open", Body: KeyMarker(hash) + FiledMarker(hash, testRunID)}}}, wantFile: 1, wantOwned: "12",
		},
		"finding marker under another key blocks approval": {
			prov: fakeProvider{open: []providers.WorkItem{{ID: "13", State: "open", Body: KeyMarker(KeyHash("other")) + FindingMarker(fp)}}}, wantFile: 1, wantUnmet: "open issue #13 already names finding",
		},
	} {
		t.Run(name, func(t *testing.T) {
			n := nom
			prov := tc.prov
			if name == "flake owner suppresses" {
				n.TestFailure = &TestFailure{Package: "p", Test: "TestX", Signature: "boom"}
				prov.flakes = []providers.WorkItem{{ID: "20", Labels: []string{FlakeLabel}, Body: "<!-- goobers-flake-fingerprint:" + flakeFP(n) + " -->"}}
			}
			plan, err := newPublisher(&prov, nil).Scan(context.Background(), pubArtifact(n))
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.File) != tc.wantFile {
				t.Fatalf("file = %d, want %d (%+v)", len(plan.File), tc.wantFile, plan)
			}
			if tc.wantSup != "" {
				if len(plan.Suppressed) != 1 || !strings.Contains(plan.Suppressed[0].Reason, tc.wantSup) {
					t.Fatalf("suppressed = %+v, want %q", plan.Suppressed, tc.wantSup)
				}
				if _, ok := plan.openDuplicates[n.Key]; ok != (tc.wantOpenID != "") {
					t.Fatalf("openDuplicates = %v", plan.openDuplicates)
				}
			}
			if tc.wantFile == 1 {
				c := plan.File[0]
				if c.OwnedIssue != tc.wantOwned {
					t.Fatalf("owned = %q, want %q", c.OwnedIssue, tc.wantOwned)
				}
				if tc.wantUnmet != "" && !strings.Contains(joined(c.ApprovalUnmet), tc.wantUnmet) {
					t.Fatalf("unmet = %v, want %q", c.ApprovalUnmet, tc.wantUnmet)
				}
			}
		})
	}
}

func TestScanBudgetOrdersByStrengthAndOverflows(t *testing.T) {
	confirmed := pubFindingNomination()
	confirmed.Key = "z-confirmed"
	confirmed.DedupeKey = "k-z"
	plain1 := Nomination{Key: "a-plain", DedupeKey: "k-a", Title: "go vet: plain nomination", Body: "go vet reports an unchecked Close result in x/y.go.", Labels: []string{"type:chore"}, RiskClass: RiskLow, RiskReason: "r",
		Evidence: []Evidence{{Kind: EvidenceSource, Path: "x/y.go", Line: 1}}}
	plain2 := plain1
	plain2.Key, plain2.DedupeKey = "b-plain", "k-b"

	pub := newPublisher(&fakeProvider{}, nil)
	pub.Policy.MaxPerRun = 2
	plan, err := pub.Scan(context.Background(), pubArtifact(plain1, plain2, confirmed))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.File) != 2 || plan.File[0].Nomination.Key != "z-confirmed" || plan.File[0].Strength != 2 || plan.File[1].Nomination.Key != "a-plain" {
		t.Fatalf("file = %+v", plan.File)
	}
	if len(plan.Overflow) != 1 || plan.Overflow[0] != "b-plain" {
		t.Fatalf("overflow = %v", plan.Overflow)
	}
}

func TestScanSecondNominationOfSameFindingIsNotApproved(t *testing.T) {
	a := pubFindingNomination()
	b := pubFindingNomination()
	b.Key, b.DedupeKey = "vet-close-2", "vet:close:2"
	res, err := newPublisher(&fakeProvider{}, &fakeApprover{}).Publish(context.Background(), pubArtifact(a, b))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Filed) != 2 || res.Approved() != 1 {
		t.Fatalf("filed = %+v", res.Filed)
	}
	if !strings.Contains(joined(res.Filed[1].ApprovalUnmet), "already names finding") {
		t.Fatalf("second unmet = %v", res.Filed[1].ApprovalUnmet)
	}
}

func TestPublishValidationAndListErrors(t *testing.T) {
	art := pubArtifact(pubFindingNomination())
	ctx := context.Background()

	for name, mutate := range map[string]func(*Publisher){
		"no provider":     func(p *Publisher) { p.Provider = nil },
		"no run id":       func(p *Publisher) { p.RunID = "" },
		"bad policy":      func(p *Publisher) { p.Policy.MaxPerRun = 0 },
		"empty label":     func(p *Publisher) { p.Policy.NominatedLabel = " " },
		"negative window": func(p *Publisher) { p.Policy.DedupeWindow = -1 },
	} {
		pub := newPublisher(&fakeProvider{}, nil)
		mutate(&pub)
		if _, err := pub.Scan(ctx, art); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	other := art
	other.RunID = "someone-else"
	if _, err := newPublisher(&fakeProvider{}, nil).Scan(ctx, other); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Errorf("foreign run id err = %v", err)
	}
	if _, err := newPublisher(&fakeProvider{listErr: errors.New("boom")}, nil).Publish(ctx, art); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("list error = %v", err)
	}
	prov := &fakeProvider{}
	if _, err := newPublisher(prov, nil).Scan(ctx, art); err != nil {
		t.Fatal(err)
	}
}

func TestPublishErrorPaths(t *testing.T) {
	ctx := context.Background()
	art := pubArtifact(pubFindingNomination())
	hash := KeyHash("vet:close")
	owned := providers.WorkItem{ID: "12", State: "open", Body: KeyMarker(hash) + FiledMarker(hash, testRunID)}

	for name, tc := range map[string]struct {
		prov *fakeProvider
		appr *fakeApprover
		want string
	}{
		"create":    {&fakeProvider{createErr: errors.New("nope")}, &fakeApprover{}, "create issue"},
		"read back": {&fakeProvider{open: []providers.WorkItem{owned}, getErr: errors.New("gone")}, &fakeApprover{}, "read issue #12"},
		"label":     {&fakeProvider{open: []providers.WorkItem{owned}, byID: map[string]providers.WorkItem{"12": {ID: "12"}}, updateErr: errors.New("denied")}, &fakeApprover{}, "label issue"},
		"approve":   {&fakeProvider{}, &fakeApprover{err: errors.New("no scope")}, "approve issue"},
	} {
		t.Run(name, func(t *testing.T) {
			pub := newPublisher(tc.prov, tc.appr)
			if _, err := pub.Publish(ctx, art); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestPublishReusesOwnedIssueAndCompletesLabels(t *testing.T) {
	hash := KeyHash("vet:close")
	prov := &fakeProvider{
		open: []providers.WorkItem{{ID: "12", State: "open", Body: KeyMarker(hash) + FiledMarker(hash, testRunID)}},
		byID: map[string]providers.WorkItem{"12": {ID: "12", Revision: "r", Labels: []string{"goobers:backlog"}}},
	}
	res, err := newPublisher(prov, &fakeApprover{}).Publish(context.Background(), pubArtifact(pubFindingNomination()))
	if err != nil {
		t.Fatal(err)
	}
	if len(prov.created) != 0 || len(prov.updated) != 1 || len(res.Filed) != 1 || !res.Filed[0].Reused || !res.Filed[0].Approved {
		t.Fatalf("created=%d updated=%d filed=%+v", len(prov.created), len(prov.updated), res.Filed)
	}
}

func TestPublishRefusesFlakeOwnedIssue(t *testing.T) {
	hash := KeyHash("vet:close")
	prov := &fakeProvider{
		open: []providers.WorkItem{{ID: "12", State: "open", Body: KeyMarker(hash) + FiledMarker(hash, testRunID)}},
		byID: map[string]providers.WorkItem{"12": {ID: "12", Labels: []string{FlakeLabel}}},
	}
	appr := &fakeApprover{}
	res, err := newPublisher(prov, appr).Publish(context.Background(), pubArtifact(pubFindingNomination()))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Refused) != 1 || len(res.Filed) != 0 || len(appr.calls) != 0 || len(prov.updated) != 0 {
		t.Fatalf("result = %+v", res)
	}
}

func TestPublishAnnotatesOpenDuplicateOncePerRun(t *testing.T) {
	nom := pubFindingNomination()
	hash := KeyHash(nom.DedupeKey)
	open := []providers.WorkItem{{ID: "5", State: "open", Body: KeyMarker(hash)}}
	ctx := context.Background()

	prov := &fakeProvider{open: open}
	res, err := newPublisher(prov, nil).Publish(ctx, pubArtifact(nom))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Annotated) != 1 || res.Annotated[0] != "5" || len(prov.commented["5"]) != 1 || !strings.Contains(prov.commented["5"][0], SeenMarker(hash, testRunID)) {
		t.Fatalf("annotated = %v comments = %v", res.Annotated, prov.commented)
	}

	prov = &fakeProvider{open: open, comments: map[string][]providers.Comment{"5": {{Body: SeenMarker(hash, testRunID)}}}}
	res, err = newPublisher(prov, nil).Publish(ctx, pubArtifact(nom))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Annotated) != 0 || len(prov.commented) != 0 {
		t.Fatalf("already-seen annotated = %v", res.Annotated)
	}

	for name, prov := range map[string]*fakeProvider{
		"list":    {open: open, commentsErr: errors.New("x")},
		"comment": {open: open, commentErr: errors.New("y")},
	} {
		if _, err := newPublisher(prov, nil).Publish(ctx, pubArtifact(nom)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestPublishObservesPublicationAndNeedsHuman(t *testing.T) {
	nom := pubFindingNomination()
	nom.RequiresHumanReview = true
	prov := &fakeProvider{}
	var seenTitle, seenBody string
	pub := newPublisher(prov, &fakeApprover{})
	pub.ObservePublication = func(_ context.Context, title, body string) { seenTitle, seenBody = title, body }
	res, err := pub.Publish(context.Background(), pubArtifact(nom))
	if err != nil {
		t.Fatal(err)
	}
	if seenTitle != nom.Title || !strings.Contains(seenBody, FiledMarker(KeyHash(nom.DedupeKey), testRunID)) {
		t.Fatalf("observed %q / %q", seenTitle, seenBody)
	}
	if !providersHas(prov.created[0].Labels, providers.LabelNeedsHuman) || res.Approved() != 0 {
		t.Fatalf("labels = %v approved = %d", prov.created[0].Labels, res.Approved())
	}
}

func providersHas(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

func TestHasSeenMarker(t *testing.T) {
	hash := KeyHash("k")
	other := KeyHash("other")
	for name, tc := range map[string]struct {
		body string
		want bool
	}{
		"match":            {"hello\n" + SeenMarker(hash, "run-1"), true},
		"match among many": {SeenMarker(other, "run-1") + "\n" + SeenMarker(hash, "run-2") + "\n" + SeenMarker(hash, "run-1"), true},
		"different run":    {SeenMarker(hash, "run-2"), false},
		"different key":    {SeenMarker(other, "run-1"), false},
		"filed marker":     {FiledMarker(hash, "run-1"), false},
		"no marker":        {"plain text", false},
		"empty":            {"", false},
	} {
		if got := hasSeenMarker(tc.body, hash, "run-1"); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}

func flakeFP(n Nomination) string {
	return flake.Fingerprint(n.TestFailure.Package, n.TestFailure.Test, flake.NormalizeSignature(n.TestFailure.Signature))
}
