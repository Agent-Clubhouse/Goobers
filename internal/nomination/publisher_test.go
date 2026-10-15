package nomination

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/flake"
	"github.com/goobers/goobers/providers"
)

type fakeProvider struct {
	open, closed, flakes []providers.WorkItem
	byID                 map[string]providers.WorkItem
	comments             map[string][]providers.Comment
	created              []providers.CreateWorkItemRequest
	updates              []providers.UpdateWorkItemRequest
	commented            []string
	listErr, createErr   error
	commentErr, getErr   error
	updateErr            error
}

func (f *fakeProvider) ListWorkItems(_ context.Context, req providers.ListWorkItemsRequest) ([]providers.WorkItem, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	if len(req.Labels) == 1 && req.Labels[0] == FlakeLabel {
		return f.flakes, nil
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
	return providers.WorkItem{ID: fmt.Sprint(100 + len(f.created)), Revision: "r1", Labels: req.Labels, State: "open"}, nil
}

func (f *fakeProvider) UpdateWorkItem(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	if f.updateErr != nil {
		return providers.WorkItem{}, f.updateErr
	}
	f.updates = append(f.updates, req)
	item := f.byID[req.ID]
	item.ID = req.ID
	item.Labels = append(append([]string(nil), item.Labels...), req.AddLabels...)
	if f.byID == nil {
		f.byID = map[string]providers.WorkItem{}
	}
	f.byID[req.ID] = item
	return item, nil
}

func (f *fakeProvider) ListComments(_ context.Context, _ providers.RepositoryRef, id string) ([]providers.Comment, error) {
	return f.comments[id], nil
}

func (f *fakeProvider) CreateWorkItemComment(_ context.Context, _ providers.RepositoryRef, id, _ string) (providers.Comment, error) {
	if f.commentErr != nil {
		return providers.Comment{}, f.commentErr
	}
	f.commented = append(f.commented, id)
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
	return providers.WorkItem{ID: req.ID, Labels: req.AddLabels}, nil
}

const vetStdout = `=== go vet (exit 1) ===
internal/worktree/manager.go:88:2: result of (*os.File).Close call not used
internal/other/x.go:3:1: other problem
`

var testNow = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

func findingEvidence() Evidence {
	return Evidence{Kind: EvidenceFinding, Tool: ToolVet, Path: "internal/worktree/manager.go", Line: 88, Rule: "result of (*os.File).Close call not used"}
}

func findingArtifact() Artifact {
	a := validArtifact()
	a.Nominations[0].Evidence = []Evidence{findingEvidence()}
	return a
}

func newPublisher(p *fakeProvider, approver Approver) Publisher {
	return Publisher{
		Provider: p, Approver: approver,
		Findings: ParseSignals([]byte(vetStdout)),
		Repo:     providers.RepositoryRef{Owner: "o", Name: "r"},
		RunID:    testRunID,
		Policy: Policy{
			BacklogLabel: "goobers:backlog", PartitionLabel: "goobers:cloud", NominatedLabel: "goobers:nominated",
			MaxPerRun: 5, DedupeWindow: 24 * time.Hour, AutoApprove: true,
		},
		Now: func() time.Time { return testNow },
	}
}

func flakeFingerprintBody(tf *TestFailure) string {
	return "<!-- goobers-flake-fingerprint:" + flake.Fingerprint(tf.Package, tf.Test, flake.NormalizeSignature(tf.Signature)) + " -->"
}

func TestPublisherRejectsBadConfiguration(t *testing.T) {
	good := newPublisher(&fakeProvider{}, nil)
	for name, mutate := range map[string]func(*Publisher){
		"no provider":      func(p *Publisher) { p.Provider = nil },
		"no run id":        func(p *Publisher) { p.RunID = "" },
		"no repo":          func(p *Publisher) { p.Repo = providers.RepositoryRef{} },
		"blank label":      func(p *Publisher) { p.Policy.BacklogLabel = " " },
		"zero max":         func(p *Publisher) { p.Policy.MaxPerRun = 0 },
		"negative window":  func(p *Publisher) { p.Policy.DedupeWindow = -1 },
		"foreign run":      func(p *Publisher) { p.RunID = "other" },
		"missing nominate": func(p *Publisher) { p.Policy.NominatedLabel = "" },
	} {
		p := good
		mutate(&p)
		if _, err := p.Scan(context.Background(), findingArtifact()); err == nil {
			t.Errorf("%s: Scan succeeded, want error", name)
		}
		if _, err := p.Publish(context.Background(), findingArtifact()); err == nil {
			t.Errorf("%s: Publish succeeded, want error", name)
		}
	}
}

func TestApprovalUnmetDecisions(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(p *Publisher, a *Artifact)
		wantAny string
	}{
		{"confirmed finding approves", func(*Publisher, *Artifact) {}, ""},
		{"findings unavailable", func(p *Publisher, _ *Artifact) {
			p.Findings, p.FindingsUnavailable = nil, "journal unreachable"
		}, "journal unreachable"},
		{"findings unavailable default reason", func(p *Publisher, _ *Artifact) { p.Findings = nil }, "not readable"},
		{"fabricated finding", func(_ *Publisher, a *Artifact) { a.Nominations[0].Evidence[0].Line = 89 }, "does not contain"},
		{"no finding evidence", func(_ *Publisher, a *Artifact) { a.Nominations[0].Evidence = validArtifact().Nominations[0].Evidence }, "no evidence names"},
		{"risk not low", func(_ *Publisher, a *Artifact) { a.Nominations[0].RiskClass = RiskHuman }, "not low"},
		{"not a bug", func(_ *Publisher, a *Artifact) { a.Nominations[0].Labels = []string{"area:runner", "type:chore"} }, "type:bug"},
		{"requires human", func(_ *Publisher, a *Artifact) { a.Nominations[0].RequiresHumanReview = true }, "requires human review"},
		{"load-bearing source", func(_ *Publisher, a *Artifact) {
			a.Nominations[0].Evidence = append(a.Nominations[0].Evidence, Evidence{Kind: EvidenceSource, Path: "internal/journal/x.go"})
		}, "load-bearing"},
		{"source outside package", func(_ *Publisher, a *Artifact) {
			a.Nominations[0].Evidence = append(a.Nominations[0].Evidence, Evidence{Kind: EvidenceSource, Path: "internal/runner/x.go"})
		}, "outside the finding's package"},
		{"two findings span packages", func(_ *Publisher, a *Artifact) {
			a.Nominations[0].Evidence = append(a.Nominations[0].Evidence, Evidence{Kind: EvidenceFinding, Tool: ToolVet, Path: "internal/other/x.go", Line: 3, Rule: "other problem"})
		}, "more than one package"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, a := newPublisher(&fakeProvider{}, nil), findingArtifact()
			tc.mutate(&p, &a)
			plan, err := p.Scan(context.Background(), a)
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.File) != 1 {
				t.Fatalf("File = %d, want 1", len(plan.File))
			}
			got := strings.Join(plan.File[0].ApprovalUnmet, "|")
			if tc.wantAny == "" {
				if got != "" {
					t.Fatalf("unmet = %q, want none", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantAny) {
				t.Fatalf("unmet = %q, want it to contain %q", got, tc.wantAny)
			}
		})
	}
}

func TestPackageDirResolution(t *testing.T) {
	for in, want := range map[string]struct {
		dir string
		ok  bool
	}{
		modulePath:                       {".", true},
		modulePath + "/internal/runner":  {"internal/runner", true},
		modulePath + "/../x":             {"", false},
		modulePath + "/a//b":             {"", false},
		"example.com/other/internal/run": {"", false},
	} {
		if dir, ok := packageDir(in); ok != want.ok || ok && dir != want.dir {
			t.Errorf("packageDir(%q) = %q,%v want %q,%v", in, dir, ok, want.dir, want.ok)
		}
	}
}

func TestDuplicateFindingUnmet(t *testing.T) {
	f := Finding{Tool: ToolVet, Path: "internal/worktree/manager.go", Line: 88, Rule: "r"}
	hash := FindingHash(f)
	recent := testNow.Add(-time.Hour)
	old := testNow.Add(-72 * time.Hour)
	body := FindingMarker(hash)
	filedByRun := KeyMarker(KeyHash("vet:k")) + FiledMarker(KeyHash("vet:k"), testRunID) + body
	cutoff := testNow.Add(-24 * time.Hour)
	cases := []struct {
		name   string
		priors priorFindings
		window time.Duration
		want   string
	}{
		{"claimed by earlier candidate", priorFindings{claimed: map[string]string{hash: "other"}}, time.Hour, `nomination "other"`},
		{"open prior", priorFindings{byFinding: map[string][]providers.WorkItem{hash: {{ID: "7", State: "open", Body: body}}}}, time.Hour, "open issue #7"},
		{"closed in window", priorFindings{cutoff: cutoff, byFinding: map[string][]providers.WorkItem{hash: {{ID: "8", State: "closed", UpdatedAt: &recent, Body: body}}}}, 24 * time.Hour, "#8 naming finding"},
		{"closed outside window", priorFindings{cutoff: cutoff, byFinding: map[string][]providers.WorkItem{hash: {{ID: "9", State: "closed", UpdatedAt: &old, Body: body}}}}, 24 * time.Hour, ""},
		{"closed with zero window", priorFindings{cutoff: cutoff, byFinding: map[string][]providers.WorkItem{hash: {{ID: "9", State: "closed", UpdatedAt: &recent, Body: body}}}}, 0, ""},
		{"this run's own filing", priorFindings{artifactKeys: map[string]bool{KeyHash("vet:k"): true}, byFinding: map[string][]providers.WorkItem{hash: {{ID: "10", State: "open", Body: filedByRun}}}}, time.Hour, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := Publisher{RunID: testRunID, Policy: Policy{DedupeWindow: tc.window}}
			got := strings.Join(p.duplicateFindingUnmet([]Finding{f}, tc.priors), "|")
			if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
				t.Fatalf("unmet = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestScanSuppressesDuplicates(t *testing.T) {
	a := findingArtifact()
	hash := KeyHash(a.Nominations[0].DedupeKey)
	recent := testNow.Add(-time.Hour)
	old := testNow.Add(-72 * time.Hour)
	cases := []struct {
		name       string
		prov       *fakeProvider
		wantSupp   int
		wantFile   int
		wantUnmet  string
		wantOwned  string
		wantReason string
	}{
		{"open duplicate", &fakeProvider{open: []providers.WorkItem{{ID: "5", State: "open", Body: KeyMarker(hash)}}}, 1, 0, "", "", "open issue #5"},
		{"closed in window", &fakeProvider{closed: []providers.WorkItem{{ID: "5", State: "closed", UpdatedAt: &recent, Body: KeyMarker(hash)}}}, 1, 0, "", "", "closed inside the dedupe window"},
		{"closed outside window", &fakeProvider{closed: []providers.WorkItem{{ID: "5", State: "closed", UpdatedAt: &old, Body: KeyMarker(hash)}}}, 0, 1, "", "", ""},
		{"flake owned", nil, 1, 0, "", "", "flake-watch already owns"},
		{"same finding under another key", &fakeProvider{open: []providers.WorkItem{{ID: "6", State: "open", Body: KeyMarker(strings.Repeat("a", 64)) + "\n" + FindingMarker(FindingHash(findingOf(findingEvidence())))}}}, 0, 1, "open issue #6 already names finding", "", ""},
		{"retry owned", &fakeProvider{open: []providers.WorkItem{{ID: "5", State: "open", Body: KeyMarker(hash) + FiledMarker(hash, testRunID)}}}, 0, 1, "", "5", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			art := a
			prov := tc.prov
			if prov == nil {
				art = validArtifact()
				art.Nominations[0].TestFailure = &TestFailure{Package: "p", Test: "TestX", Signature: "boom"}
				prov = &fakeProvider{}
			}
			p := newPublisher(prov, nil)
			if tc.name == "flake owned" {
				fp := flakeFingerprintBody(art.Nominations[0].TestFailure)
				prov.flakes = []providers.WorkItem{{ID: "9", State: "closed", Body: fp}}
			}
			plan, err := p.Scan(context.Background(), art)
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Suppressed) != tc.wantSupp || len(plan.File) != tc.wantFile {
				t.Fatalf("suppressed=%v file=%d", plan.Suppressed, len(plan.File))
			}
			if tc.wantReason != "" && !strings.Contains(plan.Suppressed[0].Reason, tc.wantReason) {
				t.Fatalf("reason = %q, want %q", plan.Suppressed[0].Reason, tc.wantReason)
			}
			if tc.wantUnmet != "" && !strings.Contains(strings.Join(plan.File[0].ApprovalUnmet, "|"), tc.wantUnmet) {
				t.Fatalf("unmet = %v, want %q", plan.File[0].ApprovalUnmet, tc.wantUnmet)
			}
			if tc.wantOwned != "" && plan.File[0].OwnedIssue != tc.wantOwned {
				t.Fatalf("owned = %q", plan.File[0].OwnedIssue)
			}
		})
	}
}

func TestScanBudgetOrdersByStrengthAndOverflows(t *testing.T) {
	a := findingArtifact()
	weak := validArtifact().Nominations[0]
	weak.Key, weak.DedupeKey = "aaa-weak", "weak:key"
	a.Nominations = append([]Nomination{weak}, a.Nominations...)
	p := newPublisher(&fakeProvider{}, nil)
	p.Policy.MaxPerRun = 1
	plan, err := p.Scan(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.File) != 1 || plan.File[0].Nomination.Key != "vet-unusedresult" || plan.File[0].Strength != 2 {
		t.Fatalf("File = %+v", plan.File)
	}
	if len(plan.Overflow) != 1 || plan.Overflow[0] != "aaa-weak" {
		t.Fatalf("Overflow = %v", plan.Overflow)
	}
}

func TestScanListErrors(t *testing.T) {
	p := newPublisher(&fakeProvider{listErr: errors.New("boom")}, nil)
	if _, err := p.Scan(context.Background(), findingArtifact()); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}

func TestPublishApprovalIsFailClosed(t *testing.T) {
	cases := []struct {
		name         string
		approver     *fakeApprover
		autoApprove  bool
		mutate       func(*Artifact)
		wantApproved bool
		wantUnmet    string
	}{
		{"approves confirmed finding", &fakeApprover{}, true, nil, true, ""},
		{"opt-in off", &fakeApprover{}, false, nil, false, "autoApprove is never on"},
		{"no approver", nil, true, nil, false, "approve credential did not resolve"},
		{"bound unmet", &fakeApprover{}, true, func(a *Artifact) { a.Nominations[0].RiskClass = RiskHuman }, false, "not low"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := &fakeProvider{}
			var ap Approver
			if tc.approver != nil {
				ap = tc.approver
			}
			p := newPublisher(prov, ap)
			p.Policy.AutoApprove = tc.autoApprove
			a := findingArtifact()
			if tc.mutate != nil {
				tc.mutate(&a)
			}
			res, err := p.Publish(context.Background(), a)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Filed) != 1 || res.Filed[0].Approved != tc.wantApproved || res.Approved() != map[bool]int{true: 1}[tc.wantApproved] {
				t.Fatalf("Filed = %+v", res.Filed)
			}
			if !tc.wantApproved {
				if !strings.Contains(strings.Join(res.Filed[0].ApprovalUnmet, "|"), tc.wantUnmet) {
					t.Fatalf("unmet = %v", res.Filed[0].ApprovalUnmet)
				}
				for _, l := range res.Filed[0].Labels {
					if l == providers.LabelApproved {
						t.Fatalf("unapproved issue carries %s", l)
					}
				}
				if tc.approver != nil && len(tc.approver.calls) != 0 {
					t.Fatalf("approver called: %+v", tc.approver.calls)
				}
				return
			}
			if len(tc.approver.calls) != 1 || tc.approver.calls[0].AddLabels[0] != providers.LabelApproved {
				t.Fatalf("approver calls = %+v", tc.approver.calls)
			}
			if len(prov.created) != 1 || prov.created[0].RunID != CreateRunID(KeyHash("vet:internal/worktree:unusedresult"), testRunID) {
				t.Fatalf("created = %+v", prov.created)
			}
			for _, l := range prov.created[0].Labels {
				if l == providers.LabelApproved {
					t.Fatal("write credential must never apply goobers:approved")
				}
			}
		})
	}
}

func TestPublishLabelsAndObserver(t *testing.T) {
	prov := &fakeProvider{}
	p := newPublisher(prov, nil)
	var observed string
	p.ObservePublication = func(_ context.Context, title, _ string) { observed = title }
	a := findingArtifact()
	a.Nominations[0].RiskClass = RiskHuman
	if _, err := p.Publish(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if observed != a.Nominations[0].Title {
		t.Fatalf("observed = %q", observed)
	}
	for _, want := range []string{"goobers:backlog", "goobers:cloud", "goobers:nominated", "area:runner", "type:bug", providers.LabelNeedsHuman} {
		if !contains(prov.created[0].Labels, want) {
			t.Errorf("labels %v missing %s", prov.created[0].Labels, want)
		}
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestPublishRetryReadsBackAndCompletesLabels(t *testing.T) {
	a := findingArtifact()
	hash := KeyHash(a.Nominations[0].DedupeKey)
	owned := providers.WorkItem{ID: "5", Revision: "r", State: "open", Labels: []string{"goobers:nominated"}, Body: KeyMarker(hash) + FiledMarker(hash, testRunID)}
	prov := &fakeProvider{open: []providers.WorkItem{owned}, byID: map[string]providers.WorkItem{"5": owned}}
	ap := &fakeApprover{}
	res, err := newPublisher(prov, ap).Publish(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if len(prov.created) != 0 {
		t.Fatal("retry must not create")
	}
	if len(prov.updates) != 1 || !contains(prov.updates[0].AddLabels, "goobers:backlog") || contains(prov.updates[0].AddLabels, "goobers:nominated") {
		t.Fatalf("updates = %+v", prov.updates)
	}
	if len(res.Filed) != 1 || !res.Filed[0].Reused || !res.Filed[0].Approved {
		t.Fatalf("Filed = %+v", res.Filed)
	}
}

func TestPublishRefusesFlakeOwnedIssue(t *testing.T) {
	a := findingArtifact()
	hash := KeyHash(a.Nominations[0].DedupeKey)
	owned := providers.WorkItem{ID: "5", State: "open", Labels: []string{FlakeLabel}, Body: KeyMarker(hash) + FiledMarker(hash, testRunID)}
	prov := &fakeProvider{open: []providers.WorkItem{owned}, byID: map[string]providers.WorkItem{"5": owned}}
	ap := &fakeApprover{}
	res, err := newPublisher(prov, ap).Publish(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Refused) != 1 || len(res.Filed) != 0 || len(ap.calls) != 0 || len(prov.updates) != 0 {
		t.Fatalf("res = %+v", res)
	}
}

func TestPublishErrors(t *testing.T) {
	a := findingArtifact()
	hash := KeyHash(a.Nominations[0].DedupeKey)
	owned := providers.WorkItem{ID: "5", State: "open", Body: KeyMarker(hash) + FiledMarker(hash, testRunID)}
	boom := errors.New("boom")
	cases := map[string]struct {
		prov *fakeProvider
		ap   *fakeApprover
		want string
	}{
		"create":    {&fakeProvider{createErr: boom}, &fakeApprover{}, "create issue"},
		"read back": {&fakeProvider{open: []providers.WorkItem{owned}, getErr: boom}, &fakeApprover{}, "read issue #5"},
		"label":     {&fakeProvider{open: []providers.WorkItem{owned}, byID: map[string]providers.WorkItem{"5": owned}, updateErr: boom}, &fakeApprover{}, "label issue"},
		"approve":   {&fakeProvider{}, &fakeApprover{err: boom}, "approve issue"},
		"annotate":  {&fakeProvider{open: []providers.WorkItem{{ID: "7", State: "open", Body: KeyMarker(hash)}}, commentErr: boom}, nil, "comment on #7"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var ap Approver
			if tc.ap != nil {
				ap = tc.ap
			}
			_, err := newPublisher(tc.prov, ap).Publish(context.Background(), a)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !errors.Is(err, boom) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestPublishAnnotatesOpenDuplicateOnce(t *testing.T) {
	a := findingArtifact()
	hash := KeyHash(a.Nominations[0].DedupeKey)
	dup := providers.WorkItem{ID: "7", State: "open", Body: KeyMarker(hash)}
	prov := &fakeProvider{open: []providers.WorkItem{dup}}
	res, err := newPublisher(prov, nil).Publish(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Annotated) != 1 || res.Annotated[0] != "7" || len(prov.commented) != 1 || len(prov.created) != 0 {
		t.Fatalf("res = %+v commented = %v", res, prov.commented)
	}

	prov = &fakeProvider{
		open:     []providers.WorkItem{dup},
		comments: map[string][]providers.Comment{"7": {{Body: SeenMarker(hash, testRunID)}}},
	}
	res, err = newPublisher(prov, nil).Publish(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Annotated) != 0 || len(prov.commented) != 0 {
		t.Fatalf("already-seen duplicate was annotated: %+v", res)
	}
}

func TestHasSeenMarker(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	other := strings.Repeat("cd", 32)
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"match", "note\n" + SeenMarker(hash, "run-1"), true},
		{"match among several", SeenMarker(other, "run-1") + SeenMarker(hash, "run-2") + SeenMarker(hash, "run-1"), true},
		{"other hash", SeenMarker(other, "run-1"), false},
		{"other run", SeenMarker(hash, "run-2"), false},
		{"no marker", "plain comment", false},
		{"empty", "", false},
		{"filed marker is not seen", FiledMarker(hash, "run-1"), false},
	}
	for _, tc := range cases {
		if got := hasSeenMarker(tc.body, hash, "run-1"); got != tc.want {
			t.Errorf("%s: hasSeenMarker = %v, want %v", tc.name, got, tc.want)
		}
	}
}
