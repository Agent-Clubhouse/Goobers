package workbenchservice

import (
	"context"
	"crypto/sha1" // Native Git object fixture.
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/internal/workbenchgraph"
	"github.com/goobers/goobers/internal/workbenchprovider"
	"github.com/goobers/goobers/providers"
)

func graphFixture(t *testing.T, transport roundTrip) (*Service, apiv1.Gaggle, httpapi.Principal) {
	t.Helper()
	service, g, p := documentsFixture(t, transport)
	g.Spec.Workbench.Sources = append(g.Spec.Workbench.Sources, apiv1.WorkbenchSource{Name: "items", Kind: "backlog"})
	g.Spec.InteractiveAccess.Actions = append(g.Spec.InteractiveAccess.Actions, "backlog.read")
	g.Spec.InteractiveAccess.Credentials.Backlog = "human-backlog"
	credentials := append(repositoryCredentials(), instance.InteractiveCredential{Name: "human-backlog", Provider: "github", Owner: "acme", Repository: "issues", Token: instance.TokenRef{Env: "HUMAN_BACKLOG"}})
	permissions, err := interactiveaccess.New([]apiv1.Gaggle{g}, credentials, interactiveaccess.Dependencies{Registrar: &secretRegistry{}})
	if err != nil {
		t.Fatal(err)
	}
	service.Permissions = permissions
	return service, g, p
}
func graphTransport(t *testing.T, commit string) roundTrip {
	t.Helper()
	responses := sourceResponses(t, commit)
	return func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer human-read-canary" {
			t.Fatal("automation auth reached graph")
		}
		if r.URL.Path == "/repos/acme/issues/issues" {
			if r.URL.Query().Get("per_page") != "25" {
				t.Fatal("graph backlog page not bounded")
			}
			return issueResponse(r, 200, "["+issueJSON+"]"), nil
		}
		body, ok := responses[r.URL.Path]
		if !ok {
			t.Fatalf("undeclared graph source request %s", r.URL)
		}
		return issueResponse(r, 200, body), nil
	}
}
func TestGraphServiceUsesActualAdaptersAndOneCurrentSourceSet(t *testing.T) {
	service, g, p := graphFixture(t, graphTransport(t, strings.Repeat("a", 40)))
	graph, err := service.Graph(context.Background(), p, g.Name)
	if err != nil {
		t.Fatal(err)
	}
	if graph.Generation != configDigest(&g) || graph.GaggleID != g.Name || len(graph.Nodes) != 2 || len(graph.Documents) != 1 || len(graph.Sources) != 2 || !graph.Partial {
		t.Fatalf("graph=%+v", graph)
	}
	for _, node := range graph.Nodes {
		if len(node.Observations) != 1 || node.Observations[0].Ref.GaggleID != g.Name {
			t.Fatal("source identity lost", node)
		}
	}
	for _, coverage := range graph.Sources {
		if coverage.SourceBindingID == "strategy" && (coverage.Status != "complete" || coverage.Commit != strings.Repeat("a", 40)) {
			t.Fatal(coverage)
		}
	}
}
func TestGraphServiceReportsUnreadSourcesWithoutCredentialOrFactoryFallback(t *testing.T) {
	service, g, p := graphFixture(t, graphTransport(t, strings.Repeat("a", 40)))
	g.Spec.InteractiveAccess.Actions = []apiv1.InteractiveAction{"repository.read"}
	if err := service.Permissions.Apply([]apiv1.Gaggle{g}, nil); err != nil {
		t.Fatal(err)
	}
	var factories atomic.Int32
	service.Backlog = func(context.Context, ReadBinding, interactiveaccess.Credential) (workbenchprovider.BacklogClient, error) {
		factories.Add(1)
		return nil, errors.New("provider-secret")
	}
	graph, err := service.Graph(context.Background(), p, g.Name)
	if err != nil || len(graph.Nodes) != 1 || factories.Load() != 0 {
		t.Fatalf("denied source read: %+v %v", graph, err)
	}
	for _, coverage := range graph.Sources {
		if coverage.SourceBindingID == "items" && (coverage.Status != "not-read" || !slices.Contains(coverage.Reasons, "source-not-loaded")) {
			t.Fatal(coverage)
		}
	}
	// A missing credential or missing server adapter is the same public state.
	g.Spec.InteractiveAccess.Actions = append(g.Spec.InteractiveAccess.Actions, "backlog.read")
	g.Spec.InteractiveAccess.Credentials.Backlog = "missing"
	if err := service.Permissions.Apply([]apiv1.Gaggle{g}, nil); err != nil {
		t.Fatal(err)
	}
	graph, err = service.Graph(context.Background(), p, g.Name)
	if err != nil || factories.Load() != 0 || len(graph.Nodes) != 1 {
		t.Fatal("missing human credential used fallback", err)
	}
	service.Repository = nil
	graph, err = service.Graph(context.Background(), p, g.Name)
	if err != nil || len(graph.Nodes) != 0 || !graph.Partial {
		t.Fatal("missing adapter fabricated content", err)
	}
	stranger := p
	stranger.Subject = "mallory"
	if _, err = service.Graph(context.Background(), stranger, g.Name); err == nil {
		t.Fatal("foreign human read source metadata")
	}
}
func TestGraphPolicyReloadWaitsForOneCoherentReadAndCancellationDropsAllContent(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	actual := graphTransport(t, strings.Repeat("a", 40))
	service, g, p := graphFixture(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/repos/acme/issues/issues" {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return actual(r)
	})
	type answer struct {
		graph workbenchgraph.Graph
		err   error
	}
	complete := make(chan answer, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { graph, err := service.Graph(ctx, p, g.Name); complete <- answer{graph, err} }()
	<-entered
	revoked := *g.DeepCopy()
	revoked.Spec.InteractiveAccess = nil
	applied := make(chan error, 1)
	go func() { applied <- service.Permissions.Apply([]apiv1.Gaggle{revoked}, nil) }()
	select {
	case <-applied:
		t.Fatal("reload mixed configuration during graph collection")
	case <-time.After(25 * time.Millisecond):
	}
	cancel()
	select {
	case result := <-complete:
		if result.err == nil || len(result.graph.Nodes) != 0 || len(result.graph.Documents) != 0 {
			t.Fatal("cancelled source content returned", result)
		}
	case <-time.After(time.Second):
		t.Fatal("bounded cancellation did not join")
	}
	select {
	case err := <-applied:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("policy reload did not finish")
	}
	if _, err := service.Graph(context.Background(), p, g.Name); err == nil {
		t.Fatal("revoked human retained graph authority")
	}
	close(release)
}
func TestGraphServiceRefusesMixedRepositoryCommits(t *testing.T) {
	var heads atomic.Int32
	one, two := graphTransport(t, strings.Repeat("a", 40)), graphTransport(t, strings.Repeat("c", 40))
	service, g, p := graphFixture(t, func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/git/ref/") {
			heads.Add(1)
		}
		if heads.Load() > 1 {
			return two(r)
		}
		return one(r)
	})
	extra := g.Spec.Workbench.Sources[0]
	extra.Name = "other-docs"
	extra.Paths = []string{"other.md"}
	g.Spec.Workbench.Sources = append(g.Spec.Workbench.Sources, extra)
	if err := service.Permissions.Apply([]apiv1.Gaggle{g}, nil); err != nil {
		t.Fatal(err)
	}
	graph, err := service.Graph(context.Background(), p, g.Name)
	var public *httpapi.InterventionError
	if !errors.As(err, &public) || public.Status != 409 || len(graph.Nodes) != 0 {
		t.Fatalf("incoherent repository exposed: %+v %v", graph, err)
	}
}
func TestGraphSourceReadUsesEightDocumentPathsAndPreservesPartialCoverage(t *testing.T) {
	service, g, p := graphFixture(t, graphTransport(t, strings.Repeat("a", 40)))
	g.Spec.Workbench.Sources[0].Paths = []string{"plan.md", "two.md", "three.md", "four.md", "five.md", "six.md", "seven.md", "eight.md", "nine.md"}
	if err := service.Permissions.Apply([]apiv1.Gaggle{g}, nil); err != nil {
		t.Fatal(err)
	}
	// All configured paths are literal; missing tree entries remain unavailable,
	// not deleted, and only the first eight files are attempted by the adapter.
	graph, err := service.Graph(context.Background(), p, g.Name)
	if err != nil || len(graph.Documents) != 8 {
		t.Fatalf("bounded source window %+v %v", graph, err)
	}
	for _, coverage := range graph.Sources {
		if coverage.SourceBindingID == "strategy" && (coverage.Status != "partial" || !slices.Contains(coverage.Reasons, "paths-not-read")) {
			t.Fatal(coverage)
		}
	}
}

func TestGraphSourceMetadataReadPermitsEmptyConfiguration(t *testing.T) {
	service, g, p := graphFixture(t, graphTransport(t, strings.Repeat("a", 40)))
	g.Spec.Workbench = nil
	if err := service.Permissions.Apply([]apiv1.Gaggle{g}, nil); err != nil {
		t.Fatal(err)
	}
	graph, err := service.Graph(context.Background(), p, g.Name)
	if err != nil || len(graph.Sources) != 0 || graph.Generation == "" || graph.Partial {
		t.Fatalf("%+v %v", graph, err)
	}
}

func TestGraphAggregateBudgetStopsFurtherAuthorizedSourceReads(t *testing.T) {
	service, g, p := documentsFixture(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("unscoped provider fallback")
		return nil, errors.New("unavailable")
	})
	template := g.Spec.Workbench.Sources[0]
	g.Spec.Workbench.Sources = nil
	for index := range 32 {
		source := template
		source.Name = fmt.Sprintf("docs-%d", index)
		source.Paths = nil
		for file := range 8 {
			source.Paths = append(source.Paths, fmt.Sprintf("source-%d/file-%d.md", index, file))
		}
		g.Spec.Workbench.Sources = append(g.Spec.Workbench.Sources, source)
	}
	if err := service.Permissions.Apply([]apiv1.Gaggle{g}, nil); err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("x", workbench.MaxSourceBytes)
	h := sha1.New() //nolint:gosec // Native Git object fixture.
	_, _ = fmt.Fprintf(h, "blob %d\x00", len(body))
	_, _ = h.Write([]byte(body))
	client := graphLargeFileClient{body: body, blob: hex.EncodeToString(h.Sum(nil))}
	calls := 0
	service.Repository = func(context.Context, ReadBinding, interactiveaccess.Credential) (workbenchprovider.RepositoryClient, error) {
		calls++
		return client, nil
	}
	graph, err := service.Graph(context.Background(), p, g.Name)
	if err != nil {
		t.Fatal(err)
	}
	if calls == 0 || calls >= 32 || !graph.Partial {
		t.Fatalf("aggregate budget not enforced: calls=%d partial=%v", calls, graph.Partial)
	}
	unread := 0
	for _, source := range graph.Sources {
		if slices.Contains(source.Reasons, "aggregate-budget") {
			unread++
			if source.Status != "not-read" {
				t.Fatal(source)
			}
		}
	}
	if unread != 32-calls {
		t.Fatalf("budget omissions hidden: unread=%d calls=%d", unread, calls)
	}
}

type graphLargeFileClient struct{ body, blob string }

func (graphLargeFileClient) Kind() providers.ProviderKind { return providers.ProviderGitHub }
func (graphLargeFileClient) ReadSourceBranch(context.Context, providers.RepositoryRef, string) (string, error) {
	return strings.Repeat("a", 40), nil
}
func (c graphLargeFileClient) ReadRepositorySource(_ context.Context, _ providers.RepositoryRef, path, commit string) (providers.RepositorySourceFile, error) {
	return providers.RepositorySourceFile{Path: path, Commit: commit, BlobID: c.blob, Content: []byte(c.body)}, nil
}
