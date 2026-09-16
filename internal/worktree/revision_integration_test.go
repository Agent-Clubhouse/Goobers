//go:build integration

package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/workspacerevision"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func revisionSource(t *testing.T) (string, string) {
	t.Helper()
	source := newSourceRepo(t)
	for _, path := range []string{"included/data.txt", "excluded/data.txt", ".gitignore"} {
		content := path + "\n"
		if path == ".gitignore" {
			content = "*.ignored\n"
		}
		mustWriteFile(t, filepath.Join(source, filepath.FromSlash(path)), content)
	}
	runTestGit(t, source, "add", ".")
	runTestGit(t, source, "commit", "-m", "selected")
	runTestGit(t, source, "config", "uploadpack.allowFilter", "true")
	runTestGit(t, source, "config", "uploadpack.allowAnySHA1InWant", "true")
	return source, strings.TrimSpace(runTestGit(t, source, "rev-parse", "HEAD"))
}

func TestIntegrationExactRevisionVerificationRefusals(t *testing.T) {
	source, sha := revisionSource(t)
	for _, expected := range []string{strings.Repeat("f", 40), sha} {
		// Even the exact object on an attached branch is not a detached inspection.
		err := verifyRevisionHEAD(context.Background(), source, expected)
		var refusal *workspacerevision.Error
		if !errors.As(err, &refusal) || refusal.Code != workspacerevision.CodeSHAMismatch {
			t.Fatalf("mismatched or attached HEAD accepted: %v", err)
		}
	}
}

func TestIntegrationExactRevisionMaterialization(t *testing.T) {
	testdep.Require(t, "git")
	ctx := context.Background()
	for _, partial := range []bool{false, true} {
		for _, sparse := range []bool{false, true} {
			name := "full"
			if partial {
				name = "partial"
			}
			if sparse {
				name += "-sparse"
			}
			t.Run(name, func(t *testing.T) {
				source, sha := revisionSource(t)
				options := []ManagerOption{}
				if partial {
					options = append(options, WithPartialClone())
				}
				m, err := NewManager(t.TempDir(), options...)
				if err != nil {
					t.Fatal(err)
				}
				opts := CreateOptions{RepoURL: source, RunID: "one", BaseRef: sha, ExpectedSHA: sha}
				if sparse {
					opts.Sparse = []string{"included"}
				}
				wt, err := m.Create(ctx, opts)
				if err != nil {
					t.Fatal(err)
				}
				if err := verifyRevisionHEAD(ctx, wt.Path, sha); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(filepath.Join(wt.Path, "included", "data.txt")); err != nil {
					t.Fatal(err)
				}
				_, statErr := os.Stat(filepath.Join(wt.Path, "excluded", "data.txt"))
				if sparse != os.IsNotExist(statErr) {
					t.Fatalf("sparse=%v, excluded file error=%v", sparse, statErr)
				}
				if mirrorIsPartial(ctx, m.repoDirForKey(repoKey(source))) != partial {
					t.Fatal("partial policy lost")
				}
				if partial && sparse {
					excludedBlob := strings.TrimSpace(runTestGit(t, source, "rev-parse", sha+":excluded/data.txt"))
					if _, err := rawGitOutput(ctx, wt.Path, nil, "--no-lazy-fetch", "cat-file", "-e", excludedBlob); err == nil {
						t.Fatal("partial sparse checkout unnecessarily acquired excluded blob")
					}
				}
				// Moving the branch cannot rebind a later stage's exact selection.
				mustWriteFile(t, filepath.Join(source, "later"), "moving branch")
				runTestGit(t, source, "add", ".")
				runTestGit(t, source, "commit", "-m", "later")
				opts.RunID = "two"
				second, err := m.Create(ctx, opts)
				if err != nil {
					t.Fatal(err)
				}
				if err := verifyRevisionHEAD(ctx, second.Path, sha); err != nil {
					t.Fatal(err)
				}
				mustWriteFile(t, filepath.Join(wt.Path, "local"), "first stage")
				runTestGit(t, wt.Path, "add", "local")
				runTestGit(t, wt.Path, "commit", "-m", "disposable")
				if _, err := os.Stat(filepath.Join(second.Path, "local")); !os.IsNotExist(err) {
					t.Fatal("independent worktree observed another stage's changes")
				}
				for _, tree := range []*Worktree{wt, second} {
					if err := tree.Remove(ctx, RemoveOptions{}); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

func TestIntegrationExactRevisionRejectsUnavailableAndNonCommit(t *testing.T) {
	testdep.Require(t, "git")
	ctx := context.Background()
	source, sha := revisionSource(t)
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runTestGit(t, source, "tag", "-a", "annotated", "-m", "not a commit object")
	for name, object := range map[string]string{
		"blob": strings.TrimSpace(runTestGit(t, source, "rev-parse", "HEAD:README.md")),
		"tree": strings.TrimSpace(runTestGit(t, source, "rev-parse", "HEAD^{tree}")),
		"tag":  strings.TrimSpace(runTestGit(t, source, "rev-parse", "annotated")),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := m.Create(ctx, CreateOptions{RepoURL: source, RunID: name, BaseRef: object, ExpectedSHA: object})
			assertRevisionCode(t, err, workspacerevision.CodeObjectType)
		})
	}
	missing := strings.Repeat("f", 40)
	_, err = m.Create(ctx, CreateOptions{RepoURL: source, RunID: "missing", BaseRef: missing, ExpectedSHA: missing})
	assertRevisionCode(t, err, workspacerevision.CodeAcquisition)
	runTestGit(t, source, "update-ref", "refs/heads/"+missing, sha)
	_, err = m.Create(ctx, CreateOptions{RepoURL: source, RunID: "substitution", BaseRef: missing, ExpectedSHA: missing})
	assertRevisionCode(t, err, workspacerevision.CodeAcquisition)
	wt, err := m.Create(ctx, CreateOptions{RepoURL: source, RunID: "before", BaseRef: sha, ExpectedSHA: sha})
	if err != nil {
		t.Fatal(err)
	}
	assertRevisionCode(t, verifyRevisionHEAD(ctx, wt.Path, missing), workspacerevision.CodeSHAMismatch)
	if err := os.Rename(source, source+"-gone"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(source + "-gone") })
	// Even though the complete object is cached, acquisition must contact
	// the authorized source and fail; neither cache nor a branch is authority.
	_, err = m.Create(ctx, CreateOptions{RepoURL: source, RunID: "after", BaseRef: sha, ExpectedSHA: sha})
	assertRevisionCode(t, err, workspacerevision.CodeAcquisition)
	if err := wt.Remove(ctx, RemoveOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationExactRevisionParallelCoexistence(t *testing.T) {
	testdep.Require(t, "git")
	ctx := context.Background()
	source, sha := revisionSource(t)
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	created := make(chan struct{}, 2)
	release := make(chan struct{})
	for _, runID := range []string{"left", "right"} {
		wg.Go(func() {
			wt, err := m.Create(ctx, CreateOptions{RepoURL: source, RunID: runID, BaseRef: sha, ExpectedSHA: sha})
			created <- struct{}{}
			if err != nil {
				t.Error(err)
				return
			}
			<-release
			if err := verifyRevisionHEAD(ctx, wt.Path, sha); err != nil {
				t.Error(err)
			}
			if err := wt.Remove(ctx, RemoveOptions{}); err != nil {
				t.Error(err)
			}
		})
	}
	<-created
	<-created
	close(release)
	wg.Wait()
}

func TestIntegrationExactRevisionDisablesUndeclaredFilters(t *testing.T) {
	testdep.Require(t, "git")
	ctx := context.Background()
	source, _ := revisionSource(t)
	mustWriteFile(t, filepath.Join(source, ".gitattributes"), "*.txt filter=undeclared\n")
	runTestGit(t, source, "add", ".")
	runTestGit(t, source, "commit", "-m", "filter attribute")
	sha := strings.TrimSpace(runTestGit(t, source, "rev-parse", "HEAD"))
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir, err := m.exactRevisionWorkingCopy(ctx, source, sha)
	if err != nil {
		t.Fatal(err)
	}
	for _, setting := range []struct{ key, value string }{
		{"filter.undeclared.smudge", "false"},
		{"filter.undeclared.clean", "false"},
		{"filter.undeclared.required", "true"},
		{"submodule.recurse", "true"},
	} {
		runTestGit(t, dir, "config", setting.key, setting.value)
	}
	wt, err := m.Create(ctx, CreateOptions{RepoURL: source, RunID: "filtered", BaseRef: sha, ExpectedSHA: sha})
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.Remove(ctx, RemoveOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationExactRevisionPreservesAuthorizationRefusal(t *testing.T) {
	testdep.Require(t, "git")
	ctx := context.Background()
	source, sha := revisionSource(t)
	m, err := NewManager(t.TempDir(), WithGitEnvironment(func(context.Context, string) ([]string, error) {
		return nil, &workspacerevision.Error{Code: workspacerevision.CodeUnauthorized, Message: "source not authorized"}
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Create(ctx, CreateOptions{RepoURL: source, RunID: "unauthorized", BaseRef: sha, ExpectedSHA: sha})
	assertRevisionCode(t, err, workspacerevision.CodeUnauthorized)
}

func TestIntegrationPinnedRevisionDiscardAndCustody(t *testing.T) {
	testdep.Require(t, "git")
	ctx := context.Background()
	for _, partial := range []bool{false, true} {
		name := "full"
		if partial {
			name = "partial-sparse"
		}
		t.Run(name, func(t *testing.T) {
			base := newSourceRepo(t)
			source, sha := revisionSource(t)
			options := []ManagerOption{}
			if partial {
				options = append(options, WithPartialClone())
			}
			m, err := NewManager(t.TempDir(), options...)
			if err != nil {
				t.Fatal(err)
			}
			lease, err := m.AcquirePinned(ctx, PinnedOptions{RepoURL: base, RunID: "pinned", BaseRef: "main"})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := lease.Release(); err != nil {
					t.Error(err)
				}
			}()
			wt := lease.Worktree
			dirty := func() {
				mustWriteFile(t, filepath.Join(wt.Path, "README.md"), "local commit")
				runTestGit(t, wt.Path, "add", "README.md")
				runTestGit(t, wt.Path, "commit", "-m", "local")
				mustWriteFile(t, filepath.Join(wt.Path, "README.md"), "tracked dirt")
				mustWriteFile(t, filepath.Join(wt.Path, "untracked"), "untracked")
				mustWriteFile(t, filepath.Join(wt.Path, "build.ignored"), "ignored")
			}
			dirty()
			var sparse []string
			if partial {
				sparse = []string{"included"}
			}
			if err := wt.PreparePinnedRevision(ctx, source, sha, sparse); err != nil {
				t.Fatal(err)
			}
			if partial {
				excludedBlob := strings.TrimSpace(runTestGit(t, source, "rev-parse", sha+":excluded/data.txt"))
				if _, err := rawGitOutput(ctx, wt.Path, nil, "--no-lazy-fetch", "cat-file", "-e", excludedBlob); err == nil {
					t.Fatal("partial pinned checkout unnecessarily acquired excluded blob")
				}
			}
			check := func() {
				t.Helper()
				if err := verifyRevisionHEAD(ctx, wt.Path, sha); err != nil {
					t.Fatal(err)
				}
				for _, path := range []string{"untracked", "build.ignored"} {
					if _, err := os.Stat(filepath.Join(wt.Path, path)); !os.IsNotExist(err) {
						t.Fatalf("%s retained: %v", path, err)
					}
				}
				if status := strings.TrimSpace(runTestGit(t, wt.Path, "status", "--porcelain")); status != "" {
					t.Fatalf("dirty selected revision: %s", status)
				}
				if origin := strings.TrimSpace(runTestGit(t, wt.Path, "remote", "get-url", "origin")); origin != base {
					t.Fatalf("origin rerouted to %s", origin)
				}
			}
			check()
			dirty()
			if err := wt.ResetPinnedRevision(ctx, sha); err != nil {
				t.Fatal(err)
			}
			check()
			dirty()
			// Simulate a stage dying before Reset: custody handoff must
			// discard source commits instead of invoking preservation.
			if err := m.handoffPinnedState(ctx, wt.key, wt.RunID); err != nil {
				t.Fatal(err)
			}
			check()
			if got := strings.TrimSpace(runTestGit(t, source, "rev-parse", "main")); got != sha {
				t.Fatal("selected source branch changed")
			}
			if err := os.Rename(source, source+"-gone"); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(source + "-gone") })
			assertRevisionCode(t, wt.PreparePinnedRevision(ctx, source, sha, sparse), workspacerevision.CodeAcquisition)
			if err := wt.PreparePinned(ctx, PinnedPrepareOptions{BaseRef: "main", Branch: "goobers/after-revision"}); err != nil {
				t.Fatal(err)
			}
			owner, err := readPinnedCustody(filepath.Join(m.pinnedRoot, wt.key))
			if err != nil || owner.SelectedRevisionSHA != "" {
				t.Fatalf("writable custody did not clear discard-only state: %+v, %v", owner, err)
			}
		})
	}
}
