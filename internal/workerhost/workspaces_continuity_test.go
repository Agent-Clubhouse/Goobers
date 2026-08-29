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

// A gate or agentic task declaring `workspace: repo-readonly` reaches this
// provisioner with that mode — the engine threads the declaration through
// unchanged — and until this arm existed it fell to the unknown-mode
// default: a gate that used to run (as an over-privileged writable worktree)
// failed closed with `unknown workspace mode "repo-readonly"`. The arm is the
// local runner's createStageWorkspace arm: a DETACHED checkout at the pinned
// base, so it never sees the run branch's commits, never collides with the
// branch another worktree holds, and never enters the continuity record.
func TestProvisionRepoReadOnlyIsDetachedAtBase(t *testing.T) {
	repo := newFixtureRepo(t)
	base := gitOutput(t, repo, "rev-parse", "main")
	store, err := blobstore.NewDir(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := worktree.NewManager(filepath.Join(t.TempDir(), "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	p := &WorktreeWorkspaces{Manager: mgr, CloneURL: func(apiv1.RepoRef) (string, error) { return repo, nil }, Store: store}
	req := engine.WorkspaceRequest{
		RunID: "run-ro", Stage: "implement", Gaggle: "web", Workflow: "implementation",
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Mode:    apiv1.WorkspaceRepo,
	}
	// The run branch moves ahead of base first, so "at base" is
	// distinguishable from "on the run branch".
	writable, err := p.Provision(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(writable.Path(), "from-implement.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, writable.Path(), "add", "from-implement.txt")
	runGit(t, writable.Path(), "-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "-q", "-m", "implement")
	if err := writable.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}

	req.Stage = "review"
	req.Mode = apiv1.WorkspaceRepoReadOnly
	ro, err := p.Provision(context.Background(), req)
	if err != nil {
		t.Fatalf("REGRESSION: a stage declaring workspace: repo-readonly cannot be provisioned on a worker: %v", err)
	}
	defer func() { _ = ro.Remove(context.Background()) }()
	if got := gitOutput(t, ro.Path(), "rev-parse", "HEAD"); got != base {
		t.Fatalf("read-only HEAD = %s, want the pinned base %s", got, base)
	}
	if got := gitOutput(t, ro.Path(), "rev-parse", "--abbrev-ref", "HEAD"); got != "HEAD" {
		t.Fatalf("read-only checkout is on branch %q, want detached", got)
	}
	if _, err := os.Stat(filepath.Join(ro.Path(), "from-implement.txt")); !os.IsNotExist(err) {
		t.Fatalf("read-only workspace carries the run branch's commit (stat err %v); want the pinned base only", err)
	}
	if _, err := os.Stat(filepath.Join(ro.Path(), "README.md")); err != nil {
		t.Fatalf("read-only workspace has no checkout content: %v", err)
	}
	// Never a publisher: nothing reported, not even Unchanged.
	if pub, err := ro.(engine.DeltaPublisher).PublishDelta(context.Background()); err != nil || pub != (engine.WorkspaceDeltaPublication{}) {
		t.Fatalf("PublishDelta on a read-only workspace = %+v, %v; want nothing", pub, err)
	}
	// Each contradiction with "reads the pinned base" is refused by name.
	for _, tc := range []struct {
		name string
		mut  func(*engine.WorkspaceRequest)
		want string
	}{
		{"syncBase", func(r *engine.WorkspaceRequest) { r.SyncBase = true }, "syncBase requires a writable repo workspace"},
		{"rebound branch", func(r *engine.WorkspaceRequest) { r.WorkspaceBranch = "goobers/pr/7" }, "rebound branch"},
		{"delta", func(r *engine.WorkspaceRequest) {
			r.WorkspaceDelta = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
		}, "read-only stage reads the pinned base"},
	} {
		bad := req
		bad.Stage = "ro-" + strings.ReplaceAll(tc.name, " ", "-")
		tc.mut(&bad)
		if _, err := p.Provision(context.Background(), bad); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("repo-readonly + %s = %v, want a refusal naming %q", tc.name, err, tc.want)
		}
	}
}

// The contract the engine's fakeWorkspaces mirrors (its
// provisionableWorkspaceModes): every value the WorkspaceMode enum admits,
// plus the unset default, has an arm here, and an unknown value has none.
// Both defects the review found were declaration-SHAPE defects (a spelling,
// an enum value) that no mechanism ablation could surface; this table is the
// check that would have caught the enum half.
func TestProvisionAcceptsEveryDeclaredWorkspaceMode(t *testing.T) {
	repo := newFixtureRepo(t)
	mgr, err := worktree.NewManager(filepath.Join(t.TempDir(), "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	p := &WorktreeWorkspaces{
		Manager: mgr, ScratchDir: filepath.Join(t.TempDir(), "scratch"),
		CloneURL: func(apiv1.RepoRef) (string, error) { return repo, nil },
	}
	for _, mode := range []apiv1.WorkspaceMode{"", apiv1.WorkspaceRepo, apiv1.WorkspaceScratch, apiv1.WorkspaceRepoReadOnly} {
		stage := "stage-" + string(mode)
		if mode == "" {
			stage = "stage-default"
		}
		ws, err := p.Provision(context.Background(), engine.WorkspaceRequest{
			RunID: "run-enum", Stage: stage, Gaggle: "web", Workflow: "implementation",
			RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
			Mode:    mode,
		})
		if err != nil {
			t.Errorf("Provision(%q) = %v, want every enum value provisionable on a worker", mode, err)
			continue
		}
		if ws.Path() == "" {
			t.Errorf("Provision(%q) returned no path", mode)
		}
		_ = ws.Remove(context.Background())
	}
	if _, err := p.Provision(context.Background(), engine.WorkspaceRequest{RunID: "run-enum", Stage: "bad", Mode: "warp"}); err == nil || !strings.Contains(err.Error(), "unknown workspace mode") {
		t.Fatalf("Provision(\"warp\") = %v, want the unknown-mode refusal", err)
	}
}

// A stage that declares run.syncBase and commits NOTHING is not a
// publication. PublishDelta once keyed on the worktree's starting ref, which
// Create captures BEFORE the syncBase merge, so a base that advanced between
// two stages read as "this stage moved its branch": when the fast-forward
// landed the branch exactly at base the bundle range was empty and git
// refused it — a hard failure for a stage that did nothing wrong — and in
// every other case a non-producer (the implementation lane's local-ci)
// became the record's newest entry, which the engine's WF022 runtime arm
// then refuses a declared 3.0 consumer over. Publication is gated on the
// stage's OWN commits, measured from the HEAD it was handed.
func TestSyncBaseOnlyStageDoesNotPublish(t *testing.T) {
	src := t.TempDir()
	runGit(t, src, "init", "--initial-branch=main")
	runGit(t, src, "config", "user.email", "test@example.com")
	runGit(t, src, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, src, "add", "README.md")
	runGit(t, src, "commit", "-q", "-m", "initial")
	advanceBase := func(name string) {
		if err := os.WriteFile(filepath.Join(src, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, src, "add", name)
		runGit(t, src, "commit", "-q", "-m", "advance "+name)
	}

	store, err := blobstore.NewDir(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := worktree.NewManager(filepath.Join(t.TempDir(), "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	p := &WorktreeWorkspaces{Manager: mgr, CloneURL: func(apiv1.RepoRef) (string, error) { return src, nil }, Store: store, Log: &log}
	req := engine.WorkspaceRequest{
		RunID: "run-sync", Stage: "one", Gaggle: "web", Workflow: "implementation",
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Mode:    apiv1.WorkspaceRepo,
	}
	// Stage one creates the run branch at main and commits nothing.
	one, err := p.Provision(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if pub, err := one.(engine.DeltaPublisher).PublishDelta(context.Background()); err != nil || !pub.Unchanged || pub.Digest != "" {
		t.Fatalf("stage one PublishDelta = %+v, %v; want Unchanged", pub, err)
	}
	if err := one.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Base advances; stage two syncs it in and commits nothing.
	advanceBase("build-fix.txt")
	req.Stage, req.SyncBase = "two", true
	two, err := p.Provision(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(two.Path(), "build-fix.txt")); err != nil {
		t.Fatalf("syncBase did not land the advanced base: %v", err)
	}
	pub, err := two.(engine.DeltaPublisher).PublishDelta(context.Background())
	if err != nil {
		t.Fatalf("stage two PublishDelta failed on a stage that committed nothing: %v", err)
	}
	if !pub.Unchanged || pub.Digest != "" {
		t.Fatalf("stage two PublishDelta = %+v, want Unchanged: a syncBase-only stage is not a producer", pub)
	}
	if err := two.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Base advances again; stage three syncs AND commits — that IS a
	// publication, carrying its commit on top of the synced base into a pod.
	advanceBase("second-fix.txt")
	req.Stage = "three"
	three, err := p.Provision(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = three.Remove(context.Background()) }()
	if err := os.WriteFile(filepath.Join(three.Path(), "from-three.txt"), []byte("three\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, three.Path(), "add", "from-three.txt")
	runGit(t, three.Path(), "-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "-q", "-m", "three")
	head := gitOutput(t, three.Path(), "rev-parse", "HEAD")
	pub, err = three.(engine.DeltaPublisher).PublishDelta(context.Background())
	if err != nil {
		t.Fatalf("stage three PublishDelta: %v", err)
	}
	if pub.Digest == "" || pub.Unchanged || pub.Tip != head {
		t.Fatalf("stage three PublishDelta = %+v, want a digest with tip %s", pub, head)
	}
	data, err := store.Get(context.Background(), pub.Digest)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := workspacedelta.Load(data, pub.Digest)
	if err != nil {
		t.Fatal(err)
	}
	pod := filepath.Join(t.TempDir(), "pod")
	runGit(t, filepath.Dir(pod), "clone", "-q", "--branch", "main", src, pod)
	if tip, err := workspacedelta.Fetch(context.Background(), testGit{}, pod, bundle); err != nil || tip != head {
		t.Fatalf("Fetch into a pod clone = %s, %v; want %s", tip, err, head)
	}
}
