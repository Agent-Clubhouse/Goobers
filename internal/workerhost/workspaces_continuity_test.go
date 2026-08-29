package workerhost

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/testgit"
	"github.com/goobers/goobers/internal/workspacedelta"
	"github.com/goobers/goobers/internal/worktree"
)

// testGit is workspacedelta.Git over the isolated test git.
type testGit struct{}

func (testGit) Run(_ context.Context, dir string, args ...string) error {
	cmd := testgit.Command(args...)
	cmd.Dir = dir
	return cmd.Run()
}

func (testGit) Output(_ context.Context, dir string, args ...string) (string, error) {
	cmd := testgit.Command(args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// podPublish stands in for a stage POD: a fresh clone of base that commits on
// the run branch and publishes the bundle into the shared store, exactly as
// dispatch-exec does. Returns the digest and the commit SHA.
func podPublish(t *testing.T, repo, branch, file string, store blobstore.Store) (string, string) {
	t.Helper()
	pod := filepath.Join(t.TempDir(), "pod")
	runGit(t, filepath.Dir(pod), "clone", "-q", "--branch", "main", repo, pod)
	runGit(t, pod, "config", "user.name", "pod")
	runGit(t, pod, "config", "user.email", "pod@example.com")
	runGit(t, pod, "checkout", "-q", "-b", branch)
	if err := os.WriteFile(filepath.Join(pod, file), []byte(file+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, pod, "add", file)
	runGit(t, pod, "commit", "-q", "-m", "commit "+file)
	head := gitOutput(t, pod, "rev-parse", "HEAD")
	b, err := workspacedelta.Create(context.Background(), testGit{}, pod, "origin/main", "HEAD")
	if err != nil {
		t.Fatalf("Create bundle: %v", err)
	}
	if err := store.Put(context.Background(), b.Digest, b.Data); err != nil {
		t.Fatalf("Put bundle: %v", err)
	}
	return b.Digest, head
}

// #3803 option 2: a request carrying a delta lands it on the mirror's run
// branch before the worktree is cut, so a self-placed stage after a pod stage
// sees the pod's commit instead of base — the M-35 shape, closed at the seam
// it was measured at (WorktreeWorkspaces.Provision).
func TestProvisionAppliesBundleIntoMirror(t *testing.T) {
	repo := newFixtureRepo(t)
	store, err := blobstore.NewDir(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	const branch = "goobers/implementation/run-3803"
	digest, podHead := podPublish(t, repo, branch, "from-pod.txt", store)

	mgr, err := worktree.NewManager(filepath.Join(t.TempDir(), "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	p := &WorktreeWorkspaces{
		Manager:  mgr,
		CloneURL: func(apiv1.RepoRef) (string, error) { return repo, nil },
		Store:    store,
		Log:      &log,
	}
	req := engine.WorkspaceRequest{
		RunID: "run-3803", Stage: "review", Gaggle: "web", Workflow: "implementation",
		RepoRef:        apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Mode:           apiv1.WorkspaceRepo,
		WorkspaceDelta: digest,
	}
	ws, err := p.Provision(context.Background(), req)
	if err != nil {
		t.Fatalf("Provision with delta: %v", err)
	}
	if got := gitOutput(t, ws.Path(), "rev-parse", "HEAD"); got != podHead {
		t.Fatalf("worktree HEAD = %s, want the pod's commit %s (the stage would review base)", got, podHead)
	}
	if _, err := os.Stat(filepath.Join(ws.Path(), "from-pod.txt")); err != nil {
		t.Fatalf("the pod's file did not reach the worker's worktree: %v", err)
	}
	if got := gitOutput(t, ws.Path(), "rev-parse", "--abbrev-ref", "HEAD"); got != branch {
		t.Fatalf("checked-out branch = %q, want the run branch", got)
	}
	if !strings.Contains(log.String(), digest) || !strings.Contains(log.String(), "create") {
		t.Fatalf("worker log = %q, want the applied digest and the create arm named", log.String())
	}
	if err := ws.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Second consumer with the same digest: the mirror is already at the tip
	// (or ahead), so the apply is a no-op and the branch is unchanged.
	log.Reset()
	req.Stage = "local-ci"
	again, err := p.Provision(context.Background(), req)
	if err != nil {
		t.Fatalf("second Provision: %v", err)
	}
	defer func() { _ = again.Remove(context.Background()) }()
	if got := gitOutput(t, again.Path(), "rev-parse", "HEAD"); got != podHead {
		t.Fatalf("second worktree HEAD = %s, want %s", got, podHead)
	}
	if !strings.Contains(log.String(), "fast-forward") {
		t.Fatalf("worker log = %q, want the equal-tip fast-forward arm named", log.String())
	}
}

// A delta with no store to read it from, or a digest the store does not
// hold, is a refusal — never a base checkout that reports success.
func TestProvisionRefusesDeltaItCannotFetch(t *testing.T) {
	repo := newFixtureRepo(t)
	mgr, err := worktree.NewManager(filepath.Join(t.TempDir(), "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	req := engine.WorkspaceRequest{
		RunID: "run-refuse", Stage: "review", Gaggle: "web", Workflow: "implementation",
		RepoRef:        apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Mode:           apiv1.WorkspaceRepo,
		WorkspaceDelta: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
	}
	noStore := &WorktreeWorkspaces{Manager: mgr, CloneURL: func(apiv1.RepoRef) (string, error) { return repo, nil }}
	if _, err := noStore.Provision(context.Background(), req); err == nil || !strings.Contains(err.Error(), "no blob store") {
		t.Fatalf("Provision with a delta and no store = %v, want the no-store refusal", err)
	}
	store, err := blobstore.NewDir(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	withStore := &WorktreeWorkspaces{Manager: mgr, CloneURL: func(apiv1.RepoRef) (string, error) { return repo, nil }, Store: store}
	if _, err := withStore.Provision(context.Background(), req); err == nil || !strings.Contains(err.Error(), "not in the blob store") {
		t.Fatalf("Provision with an absent digest = %v, want the missing-blob refusal", err)
	}
	// Scratch never lands a delta.
	scratch := &WorktreeWorkspaces{ScratchDir: t.TempDir()}
	if _, err := scratch.Provision(context.Background(), engine.WorkspaceRequest{Stage: "s", Mode: apiv1.WorkspaceScratch, WorkspaceDelta: req.WorkspaceDelta}); err == nil || !strings.Contains(err.Error(), "scratch workspace") {
		t.Fatalf("scratch + delta = %v, want the scratch refusal", err)
	}
	// The empty-delta path is byte-identical: no store, no delta, provisions.
	req.WorkspaceDelta = ""
	ws, err := noStore.Provision(context.Background(), req)
	if err != nil {
		t.Fatalf("Provision without a delta and without a store: %v", err)
	}
	_ = ws.Remove(context.Background())
}

// The reverse direction (#3803): a self-placed stage that commits publishes
// base..HEAD into the store so the next pod can continue from it, and one
// that does not commit reports Unchanged — never a stale or empty bundle.
func TestWorktreeWorkspacePublishesDelta(t *testing.T) {
	repo := newFixtureRepo(t)
	store, err := blobstore.NewDir(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := worktree.NewManager(filepath.Join(t.TempDir(), "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	p := &WorktreeWorkspaces{Manager: mgr, CloneURL: func(apiv1.RepoRef) (string, error) { return repo, nil }, Store: store, Log: &log}
	req := engine.WorkspaceRequest{
		RunID: "run-pub", Stage: "implement", Gaggle: "web", Workflow: "implementation",
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Mode:    apiv1.WorkspaceRepo,
	}
	ws, err := p.Provision(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	publisher, ok := ws.(engine.DeltaPublisher)
	if !ok {
		t.Fatal("a repo workspace must implement engine.DeltaPublisher")
	}
	// Nothing committed yet: unchanged, nothing in the store.
	pub, err := publisher.PublishDelta(context.Background())
	if err != nil || !pub.Unchanged || pub.Digest != "" {
		t.Fatalf("PublishDelta on an untouched worktree = %+v, %v; want Unchanged", pub, err)
	}
	// The stage commits.
	if err := os.WriteFile(filepath.Join(ws.Path(), "from-worker.txt"), []byte("worker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, ws.Path(), "add", "from-worker.txt")
	runGit(t, ws.Path(), "-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "-q", "-m", "worker commit")
	head := gitOutput(t, ws.Path(), "rev-parse", "HEAD")
	base := gitOutput(t, ws.Path(), "rev-parse", "refs/heads/main")
	pub, err = publisher.PublishDelta(context.Background())
	if err != nil {
		t.Fatalf("PublishDelta: %v", err)
	}
	if pub.Digest == "" || pub.Unchanged || pub.Tip != head || pub.Base != base {
		t.Fatalf("PublishDelta = %+v, want a digest carrying %s..%s", pub, base, head)
	}
	data, err := store.Get(context.Background(), pub.Digest)
	if err != nil {
		t.Fatalf("the published bundle is not in the store: %v", err)
	}
	// A pod-style fresh clone of base lands exactly on the worker's commit.
	pod := filepath.Join(t.TempDir(), "pod")
	runGit(t, filepath.Dir(pod), "clone", "-q", "--branch", "main", repo, pod)
	bundle, err := workspacedelta.Load(data, pub.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if tip, err := workspacedelta.Fetch(context.Background(), testGit{}, pod, bundle); err != nil || tip != head {
		t.Fatalf("Fetch into a pod clone = %s, %v; want %s", tip, err, head)
	}
	if !strings.Contains(log.String(), pub.Digest) {
		t.Fatalf("worker log = %q, want the published digest", log.String())
	}
	_ = ws.Remove(context.Background())

	// No store (mode 1/2): nothing is published and nothing is claimed.
	noStore := &WorktreeWorkspaces{Manager: mgr, CloneURL: func(apiv1.RepoRef) (string, error) { return repo, nil }}
	req.Stage = "again"
	ws2, err := noStore.Provision(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws2.Remove(context.Background()) }()
	if pub, err := ws2.(engine.DeltaPublisher).PublishDelta(context.Background()); err != nil || pub != (engine.WorkspaceDeltaPublication{}) {
		t.Fatalf("PublishDelta without a store = %+v, %v; want nothing", pub, err)
	}
}
