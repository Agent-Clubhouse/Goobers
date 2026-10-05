package workbenchprovider

import (
	"context"
	"crypto/sha1" // Git object identity in in-process fixtures.
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/providers"
)

const objectiveSource = "---\ngoobers:\n  schemaVersion: objectives/v1\n  objectiveId: obj-00000000-0000-0000-0000-000000000001\n  title: Source-owned objective\n---\n# Source body\n"

func repositorySource(kind string, paths ...string) (workbench.Scope, workbench.BoundSource) {
	target := apiv1.InteractiveRepositoryIdentity{Provider: apiv1.ProviderGitHub, Owner: "org", Name: "repo"}
	return workbench.Scope{GaggleID: "g", Bindings: map[string]bool{"strategy": true, "backlog": true}}, workbench.BoundSource{Spec: apiv1.WorkbenchSource{Name: "strategy", Kind: kind, Repository: &target, Paths: paths}, Repository: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "org", Name: "repo", Branch: "main"}}
}

func fixtureSourceFile(path, commit, content string) providers.RepositorySourceFile {
	h := sha1.New() //nolint:gosec // Native Git object fixture.
	_, _ = fmt.Fprintf(h, "blob %d\x00", len(content))
	_, _ = h.Write([]byte(content))
	return providers.RepositorySourceFile{Path: path, Commit: commit, BlobID: hex.EncodeToString(h.Sum(nil)), Content: []byte(content)}
}

func TestRepositoryPageComposesActualGitHubSourceAndObjectiveParser(t *testing.T) {
	commit, tree := strings.Repeat("a", 40), strings.Repeat("b", 40)
	file := fixtureSourceFile("plan.md", commit, objectiveSource)
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		var value interface{}
		switch req.URL.Path {
		case "/repos/org/repo/git/ref/heads/main":
			value = map[string]interface{}{"ref": "refs/heads/main", "object": map[string]string{"sha": commit}}
		case "/repos/org/repo/git/commits/" + commit:
			value = map[string]interface{}{"sha": commit, "tree": map[string]string{"sha": tree}}
		case "/repos/org/repo/git/trees/" + tree:
			value = map[string]interface{}{"sha": tree, "tree": []map[string]interface{}{{"path": "plan.md", "type": "blob", "mode": "100644", "sha": file.BlobID, "size": len(file.Content)}}}
		case "/repos/org/repo/git/blobs/" + file.BlobID:
			value = map[string]interface{}{"sha": file.BlobID, "size": len(file.Content), "encoding": "base64", "content": base64.StdEncoding.EncodeToString(file.Content)}
		default:
			t.Fatalf("unexpected request %s", req.URL)
			return nil, errors.New("unexpected")
		}
		raw, _ := json.Marshal(value)
		return response(string(raw)), nil
	})}
	scope, bound := repositorySource("documents", "plan.md")
	reader, err := NewRepositoryReader(scope, bound, providers.NewGitHubProvider("", providers.WithHTTPClient(client)))
	if err != nil {
		t.Fatal(err)
	}
	page, err := reader.Page(context.Background(), workbench.DocumentPageRequest{})
	if err != nil || calls != 4 || page.Coverage != "complete" || !page.Exhausted || len(page.Files) != 1 {
		t.Fatalf("page %+v %v calls=%d", page, err, calls)
	}
	got := page.Files[0]
	if got.Status != "available" || got.Ref == nil || got.Ref.SourceID != "obj-00000000-0000-0000-0000-000000000001" || got.Provenance.Commit != commit || got.Provenance.BlobID != file.BlobID || got.Provenance.ContentDigest == "" || got.Body != "# Source body\n" {
		t.Fatalf("parsed source %+v", got)
	}
}

type sourceClient struct {
	commit string
	files  map[string]string
	reads  []string
	tamper bool
}

func (*sourceClient) Kind() providers.ProviderKind { return providers.ProviderGitHub }
func (s *sourceClient) ReadSourceBranch(context.Context, providers.RepositoryRef, string) (string, error) {
	return s.commit, nil
}
func (s *sourceClient) ReadRepositorySource(ctx context.Context, _ providers.RepositoryRef, name, commit string) (providers.RepositorySourceFile, error) {
	if _, ok := ctx.Deadline(); !ok {
		return providers.RepositorySourceFile{}, errors.New("missing deadline")
	}
	s.reads = append(s.reads, name)
	content, found := s.files[name]
	if !found {
		return providers.RepositorySourceFile{}, errors.New("missing or inaccessible")
	}
	file := fixtureSourceFile(name, commit, content)
	if s.tamper {
		file.Content = append(file.Content, '!')
	}
	return file, nil
}

func TestRepositoryCursorCannotSelectHistoryOrUndeclaredPath(t *testing.T) {
	scope, bound := repositorySource("documents", "one.md", "two.md")
	client := &sourceClient{commit: strings.Repeat("a", 40), files: map[string]string{"one.md": "one", "two.md": "two", "secret.md": "secret"}}
	reader, _ := NewRepositoryReader(scope, bound, client)
	page, err := reader.Page(context.Background(), workbench.DocumentPageRequest{Limit: 1})
	if err != nil || page.Exhausted || page.Coverage != "partial" || page.NextCursor == "" || len(client.reads) != 1 {
		t.Fatalf("first window %+v %v", page, err)
	}
	last, err := reader.Page(context.Background(), workbench.DocumentPageRequest{Cursor: page.NextCursor})
	if err != nil || !last.Exhausted || last.Coverage != "partial" || last.StartOffset != 1 || len(client.reads) != 2 {
		t.Fatalf("second window %+v %v", last, err)
	}
	client.commit = strings.Repeat("b", 40)
	_, err = reader.Page(context.Background(), workbench.DocumentPageRequest{Cursor: page.NextCursor})
	if !errors.Is(err, ErrSourceChanged) || len(client.reads) != 2 {
		t.Fatalf("cursor granted historical access %v reads=%v", err, client.reads)
	}
	foreign := bound
	foreign.Repository.Name = "other"
	target := *foreign.Spec.Repository
	target.Name = "other"
	foreign.Spec.Repository = &target
	other, _ := NewRepositoryReader(scope, foreign, client)
	_, err = other.Page(context.Background(), workbench.DocumentPageRequest{Cursor: page.NextCursor})
	if !errors.Is(err, ErrInvalidCursor) || len(client.reads) != 2 {
		t.Fatalf("foreign cursor read %v", err)
	}
}

func TestRepositoryMissingInvalidAndTamperedSourcesStayPartial(t *testing.T) {
	scope, bound := repositorySource("documents", "ordinary.md", "missing.md", "invalid.md")
	client := &sourceClient{commit: strings.Repeat("a", 40), files: map[string]string{"ordinary.md": "ordinary source is not an objective", "invalid.md": "---\ngoobers:\n  schemaVersion: broken\n---\ninvalid"}}
	reader, _ := NewRepositoryReader(scope, bound, client)
	page, err := reader.Page(context.Background(), workbench.DocumentPageRequest{})
	if err != nil || !page.Exhausted || page.Coverage != "partial" || len(page.Files) != 3 {
		t.Fatalf("partial page %+v %v", page, err)
	}
	if page.Files[0].Ref != nil || page.Files[0].Status != "available" || page.Files[1].Status != "unavailable" || page.Files[2].Status != "invalid-source" {
		t.Fatalf("false source inference %+v", page.Files)
	}
	client.tamper = true
	page, err = reader.Page(context.Background(), workbench.DocumentPageRequest{})
	if err != nil || page.Files[0].Status != "unavailable" || page.Files[0].Body != "" {
		t.Fatalf("tampered bytes surfaced %+v %v", page, err)
	}
}

func TestRepositoryManifestUsesClosedSourceContract(t *testing.T) {
	manifest := `schemaVersion: relationships/v1
edges:
  - edgeId: edge-00000000-0000-0000-0000-000000000001
    kind: contributes-to
    from:
      gaggleId: g
      sourceBindingId: backlog
      kind: work-item
      sourceId: "101"
    to:
      gaggleId: g
      sourceBindingId: strategy
      kind: objective-document
      sourceId: obj-00000000-0000-0000-0000-000000000001
`
	scope, bound := repositorySource("relationships", "links.yaml")
	client := &sourceClient{commit: strings.Repeat("a", 40), files: map[string]string{"links.yaml": manifest}}
	reader, _ := NewRepositoryReader(scope, bound, client)
	page, err := reader.Page(context.Background(), workbench.DocumentPageRequest{})
	if err != nil || page.Coverage != "complete" || page.Files[0].Manifest == nil || len(page.Files[0].Manifest.Edges) != 1 {
		t.Fatalf("manifest %+v %v", page, err)
	}
	client.files["links.yaml"] = strings.ReplaceAll(manifest, "gaggleId: g", "gaggleId: foreign")
	page, err = reader.Page(context.Background(), workbench.DocumentPageRequest{})
	if err != nil || page.Coverage != "partial" || page.Files[0].Manifest != nil {
		t.Fatalf("foreign edges exposed %+v %v", page, err)
	}
}

func TestRepositoryPageHasAggregateByteBoundAndContinuation(t *testing.T) {
	names := []string{"one.md", "two.md", "three.md", "four.md", "five.md"}
	scope, bound := repositorySource("documents", names...)
	client := &sourceClient{commit: strings.Repeat("a", 40), files: map[string]string{}}
	for _, name := range names {
		client.files[name] = strings.Repeat("x", workbench.MaxSourceBytes)
	}
	reader, _ := NewRepositoryReader(scope, bound, client)
	page, err := reader.Page(context.Background(), workbench.DocumentPageRequest{})
	raw, _ := json.Marshal(page)
	if err != nil || len(raw) > workbench.MaxDocumentPageBytes || len(page.Files) != 3 || page.NextCursor == "" || page.Exhausted {
		t.Fatalf("byte bound files=%d len=%d err=%v", len(page.Files), len(raw), err)
	}
	next, err := reader.Page(context.Background(), workbench.DocumentPageRequest{Cursor: page.NextCursor})
	if err != nil || len(next.Files) != 2 || !next.Exhausted || next.StartOffset != 3 {
		t.Fatalf("byte-limited path skipped %+v %v", next, err)
	}
}

func TestRepositoryReadCancellationAndInvalidDeclarations(t *testing.T) {
	scope, bound := repositorySource("documents", "plan.md")
	client := &sourceClient{commit: strings.Repeat("a", 40), files: map[string]string{"plan.md": objectiveSource}}
	reader, err := NewRepositoryReader(scope, bound, client)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	page, err := reader.Page(ctx, workbench.DocumentPageRequest{})
	if !errors.Is(err, context.Canceled) || len(page.Files) != 0 || len(client.reads) != 0 {
		t.Fatalf("cancelled source surfaced %+v %v", page, err)
	}
	for _, name := range []string{"../plan.md", "/plan.md", "https://other/plan.md", "*.md", strings.Repeat("d/", providers.MaxRepositorySourceDepth) + "plan.md"} {
		invalid := bound
		invalid.Spec.Paths = []string{name}
		if _, err := NewRepositoryReader(scope, invalid, client); !errors.Is(err, ErrInvalidSource) {
			t.Fatalf("invalid path accepted %q: %v", name, err)
		}
	}
}
