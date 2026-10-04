package workbenchservice

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/workbench"
)

type proposalProvider struct {
	t                                                       *testing.T
	mu                                                      sync.Mutex
	gets, posts                                             int
	responses                                               map[string]string
	tree, commit, content, message, branch, prBody, prTitle string
	lose                                                    string
	postHook                                                func(*http.Request)
}

func (s *proposalProvider) roundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer human-read-canary" {
		s.t.Fatal("not exact interactive identity")
	}
	if r.URL.Host != "api.github.com" || !strings.HasPrefix(r.URL.Path, "/repos/acme/code/") {
		s.t.Fatal("foreign target", r.URL)
	}
	path := strings.TrimPrefix(r.URL.Path, "/repos/acme/code/")
	if r.Method == "GET" {
		s.gets++
		if body, ok := s.responses[r.URL.Path]; ok {
			return issueResponse(r, 200, body), nil
		}
		switch path {
		case "git/trees/" + s.tree:
			return s.json(r, map[string]any{"sha": s.tree, "tree": []map[string]any{{"path": "plan.md", "mode": "100644", "type": "blob", "sha": proposalBlob(s.content), "size": len(s.content)}}})
		case "git/commits/" + s.commit:
			return s.json(r, s.commitBody())
		case "git/blobs/" + proposalBlob(s.content):
			return s.json(r, map[string]any{"sha": proposalBlob(s.content), "encoding": "base64", "size": len(s.content), "content": base64.StdEncoding.EncodeToString([]byte(s.content))})
		case "git/ref/heads/" + s.branch:
			return s.json(r, map[string]any{"ref": "refs/heads/" + s.branch, "object": map[string]string{"sha": s.commit}})
		case "pulls":
			if r.URL.Query().Get("head") != "acme:"+s.branch || r.URL.Query().Get("base") != "strategy" || r.URL.Query().Get("per_page") != "2" {
				s.t.Fatal("unbounded observation", r.URL)
			}
			return s.json(r, []any{s.pr()})
		}
	}
	if r.Method != "POST" {
		s.t.Fatal("unexpected source effect", r.Method, r.URL)
	}
	s.posts++
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.t.Fatal(err)
	}
	var result any
	switch path {
	case "git/trees":
		entries := body["tree"].([]any)
		if len(entries) != 1 || body["base_tree"] != strings.Repeat("b", 40) {
			s.t.Fatal("arbitrary tree", body)
		}
		entry := entries[0].(map[string]any)
		if entry["path"] != "plan.md" || entry["mode"] != "100644" {
			s.t.Fatal(entry)
		}
		s.content = entry["content"].(string)
		result = map[string]string{"sha": s.tree}
	case "git/commits":
		if parents := body["parents"].([]any); len(parents) != 1 || parents[0] != strings.Repeat("a", 40) || body["tree"] != s.tree {
			s.t.Fatal("ancestry changed", body)
		}
		s.message = body["message"].(string)
		result = s.commitBody()
	case "git/refs":
		if s.branch != "" || body["sha"] != s.commit {
			s.t.Fatal("duplicate or wrong branch effect", body)
		}
		s.branch = strings.TrimPrefix(body["ref"].(string), "refs/heads/")
		if !strings.HasPrefix(s.branch, "goobers/workbench/") {
			s.t.Fatal("default branch write", body)
		}
		result = map[string]any{"ref": body["ref"], "object": map[string]string{"sha": s.commit}}
	case "pulls":
		if body["draft"] != true || body["base"] != "strategy" || body["head"] != s.branch {
			s.t.Fatal("unsafe PR", body)
		}
		s.prTitle, s.prBody = body["title"].(string), body["body"].(string)
		result = s.pr()
	default:
		s.t.Fatal("unexpected POST", path)
	}
	if s.postHook != nil {
		s.postHook(r)
	}
	if s.lose == path {
		return nil, io.ErrUnexpectedEOF
	}
	return s.json(r, result)
}
func (s *proposalProvider) json(r *http.Request, value any) (*http.Response, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		s.t.Fatal(err)
	}
	return issueResponse(r, 200, string(raw)), nil
}
func (s *proposalProvider) commitBody() any {
	return map[string]any{"sha": s.commit, "message": s.message, "tree": map[string]string{"sha": s.tree}, "parents": []map[string]string{{"sha": strings.Repeat("a", 40)}}}
}
func (s *proposalProvider) pr() any {
	repo := map[string]string{"full_name": "acme/code"}
	return map[string]any{"number": 7, "title": s.prTitle, "body": s.prBody, "draft": true, "state": "open", "head": map[string]any{"ref": s.branch, "sha": s.commit, "repo": repo}, "base": map[string]any{"ref": "strategy", "repo": repo}}
}
func proposalBlob(raw string) string {
	sum := sha1.Sum(append(fmt.Appendf(nil, "blob %d\x00", len(raw)), []byte(raw)...))
	return fmt.Sprintf("%x", sum)
}
func proposalServiceFixture(t *testing.T) (*ProposalService, *proposalProvider, apiv1.Gaggle, httpapi.Principal, workbench.MetadataChangeRequest) {
	t.Helper()
	f := &proposalProvider{t: t, responses: sourceResponses(t, strings.Repeat("a", 40)), tree: strings.Repeat("c", 40), commit: strings.Repeat("d", 40)}
	read, g, p := documentsFixture(t, f.roundTrip)
	g.Spec.Workbench.Sources[0].Writes = &apiv1.WorkbenchWrites{Fields: []apiv1.WorkbenchField{"title", "description"}}
	g.Spec.InteractiveAccess.Actions = append(g.Spec.InteractiveAccess.Actions, "source.proposeChange")
	if err := read.Permissions.Apply([]apiv1.Gaggle{g}, nil); err != nil {
		t.Fatal(err)
	}
	queue, err := triggerqueue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	factory := ProviderFactory{SchedulerDirectory: t.TempDir(), Client: &http.Client{Transport: roundTrip(f.roundTrip)}, Registrar: &secretRegistry{}}
	service := &ProposalService{ReadService: read, Queue: queue, Provider: factory.RepositoryProposal}
	value := "Human-reviewed objective"
	request := workbench.MetadataChangeRequest{Path: "plan.md", Expected: workbench.MetadataRevision{Commit: strings.Repeat("a", 40), BlobID: proposalBlob(documentSource), ContentDigest: fmt.Sprintf("%x", sha256.Sum256([]byte(documentSource)))}, Field: "title", Value: &value}
	return service, f, g, p, request
}
func TestProposalServiceRealProviderPreviewAndConfirmedReplay(t *testing.T) {
	s, f, g, p, request := proposalServiceFixture(t)
	preview, err := s.Preview(t.Context(), p, g.Name, "strategy", request)
	if err != nil || !preview.Changed || f.posts != 0 {
		t.Fatal(preview, err)
	}
	command, err := s.Submit(t.Context(), p, g.Name, "strategy", "human-key", request)
	if err != nil || command.State != "confirmed" || f.posts != 4 || len(command.Phases) != 4 {
		t.Fatal(command, err, f.posts)
	}
	if command.Phases[3].PullRequest.URL != "https://github.com/acme/code/pull/7" || command.Actor.Subject != p.Subject || f.content != preview.After {
		t.Fatal(command)
	}
	calls := f.gets
	t.Setenv("HUMAN_BACKLOG", "")
	replay, err := s.Submit(t.Context(), p, g.Name, "strategy", "human-key", request)
	if err != nil || !replay.Duplicate || replay.ID != command.ID || f.gets != calls || f.posts != 4 {
		t.Fatal(replay, err)
	}
	got, err := s.Command(t.Context(), p, g.Name, "strategy", command.ID)
	if err != nil || got.State != "confirmed" || f.gets != calls {
		t.Fatal(got, err)
	}
}
func TestProposalServiceUnknownObservationsNeverAdvanceAndReceiptsRemainImmutable(t *testing.T) {
	s, f, g, p, request := proposalServiceFixture(t)
	f.lose = "git/refs"
	first, err := s.Submit(t.Context(), p, g.Name, "strategy", "branch-lost", request)
	if err != nil || first.State != "unknown" || f.posts != 3 {
		t.Fatal(first, err, f.posts)
	}
	for range 2 {
		if _, err = s.Submit(t.Context(), p, g.Name, "strategy", "branch-lost", request); err != nil {
			t.Fatal(err)
		}
	}
	if f.posts != 3 {
		t.Fatal("unknown branch replayed")
	}
	checked, err := s.Check(t.Context(), p, g.Name, "strategy", first.ID)
	if err != nil || checked.State != "prepared" || f.posts != 3 || checked.Phases[2].Outcome != "unknown" {
		t.Fatal(checked, err)
	}
	if _, err = s.Check(t.Context(), p, g.Name, "strategy", first.ID); err != nil || f.posts != 3 {
		t.Fatal("Check advanced phase", err)
	}
	f.lose = "pulls"
	continued, err := s.Continue(t.Context(), p, g.Name, "strategy", first.ID)
	if err != nil || continued.State != "unknown" || f.posts != 4 {
		t.Fatal(continued, err)
	}
	// Revoked writes still permit exact actor-scoped read observation.
	g.Spec.Workbench.Sources[0].Writes = nil
	g.Spec.InteractiveAccess.Actions = []apiv1.InteractiveAction{"repository.read"}
	if err = s.ReadService.Permissions.Apply([]apiv1.Gaggle{g}, nil); err != nil {
		t.Fatal(err)
	}
	checked, err = s.Check(t.Context(), p, g.Name, "strategy", first.ID)
	if err != nil || checked.State != "observed" || checked.Phases[3].Outcome != "unknown" || f.posts != 4 {
		t.Fatal(checked, err)
	}
	if _, err = s.Continue(t.Context(), p, g.Name, "strategy", first.ID); err == nil {
		t.Fatal("read authority restored write")
	}
	raw, _ := json.Marshal(checked)
	if strings.Contains(string(raw), "human-read-canary") || strings.Contains(string(raw), "Before") || strings.Contains(string(raw), "Human-reviewed objective") {
		t.Fatal("receipt leaked source or credential")
	}
}
func TestProposalServiceCurrentActorAndPhysicalSourceGateReceipt(t *testing.T) {
	s, f, g, p, request := proposalServiceFixture(t)
	first, err := s.Submit(t.Context(), p, g.Name, "strategy", "request", request)
	if err != nil {
		t.Fatal(err)
	}
	calls := f.gets
	for _, change := range []func(*apiv1.Gaggle){func(g *apiv1.Gaggle) { g.Spec.Project.Branch = "other" }, func(g *apiv1.Gaggle) { g.Spec.Workbench.Sources[0].Paths = []string{"other.md"} }, func(g *apiv1.Gaggle) { g.Spec.InteractiveAccess.Credentials.Repositories = nil }} {
		changed := g.DeepCopy()
		change(changed)
		if err = s.ReadService.Permissions.Apply([]apiv1.Gaggle{*changed}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err = s.Command(t.Context(), p, g.Name, "strategy", first.ID); err == nil {
			t.Fatal("changed source exposed custody")
		}
	}
	if err = s.ReadService.Permissions.Apply([]apiv1.Gaggle{g}, nil); err != nil {
		t.Fatal(err)
	}
	p.Subject = "other"
	if _, err = s.Command(t.Context(), p, g.Name, "strategy", first.ID); err == nil {
		t.Fatal("foreign actor read receipt")
	}
	if f.gets != calls {
		t.Fatal("receipt authorization made provider request")
	}
}
func TestProposalServiceStalePreviewCannotMutateOrRetryRejectedCommand(t *testing.T) {
	s, f, g, p, request := proposalServiceFixture(t)
	request.Expected.Commit = strings.Repeat("e", 40)
	if _, err := s.Preview(t.Context(), p, g.Name, "strategy", request); err == nil {
		t.Fatal("stale preview")
	}
	first, err := s.Submit(t.Context(), p, g.Name, "strategy", "stale", request)
	if err != nil || first.State != "not-applied" || f.posts != 0 {
		t.Fatal(first, err)
	}
	gets := f.gets
	if _, err = s.Submit(t.Context(), p, g.Name, "strategy", "stale", request); err != nil || f.gets != gets {
		t.Fatal("rejected command retried", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = s.Command(ctx, p, g.Name, "strategy", first.ID); err == nil || errors.Is(err, context.Canceled) {
		t.Fatal("missing safe cancellation error", err)
	}
}

func TestProposalServiceCancellationRetainsAttemptBeforeReleasingAuthority(t *testing.T) {
	s, f, g, p, request := proposalServiceFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.lose = "git/refs"
	f.postHook = func(r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "git/refs") {
			cancel()
		}
	}
	command, err := s.Submit(ctx, p, g.Name, "strategy", "cancelled", request)
	if err != nil || command.State != "unknown" || len(command.Phases) != 3 || f.posts != 3 {
		t.Fatal(command, err)
	}
	retained, err := s.Command(t.Context(), p, g.Name, "strategy", command.ID)
	if err != nil || retained.Phases[2].Outcome != "unknown" || f.posts != 3 {
		t.Fatal(retained, err)
	}
}

func TestProposalServiceConcurrentSubmitCannotClaimSecondProviderAttempt(t *testing.T) {
	s, f, g, p, request := proposalServiceFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f.postHook = func(r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "git/trees") {
			close(entered)
			<-release
		}
	}
	type outcome struct {
		command workbench.MetadataProposalCommand
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		command, err := s.Submit(t.Context(), p, g.Name, "strategy", "concurrent", request)
		done <- outcome{command, err}
	}()
	<-entered
	duplicate, err := s.Submit(t.Context(), p, g.Name, "strategy", "concurrent", request)
	if err != nil || !duplicate.Duplicate || duplicate.State != "attempting" || len(duplicate.Phases) != 1 {
		t.Fatal(duplicate, err)
	}
	close(release)
	final := <-done
	if final.err != nil || final.command.State != "confirmed" || f.posts != 4 {
		t.Fatal(final, f.posts)
	}
}

func TestProposalServiceExpiredReceiptStillRequiresExactConfiguredTarget(t *testing.T) {
	s, _, g, p, request := proposalServiceFixture(t)
	command, err := s.Submit(t.Context(), p, g.Name, "strategy", "retained", request)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.Queue.PruneWorkbenchCommands(t.Context(), command.CompletedAt.Add(triggerqueue.WorkbenchCommandRetention+time.Hour), 100); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	_, err = s.Command(t.Context(), p, g.Name, "strategy", command.ID)
	expectWriteStatus(t, err, http.StatusGone)
	g.Spec.Project.Branch = "retargeted"
	if err = s.ReadService.Permissions.Apply([]apiv1.Gaggle{g}, nil); err != nil {
		t.Fatal(err)
	}
	_, err = s.Command(t.Context(), p, g.Name, "strategy", command.ID)
	expectWriteStatus(t, err, http.StatusForbidden)
}
