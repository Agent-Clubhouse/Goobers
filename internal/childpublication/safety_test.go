package childpublication

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/providers"
)

func TestPublicationSafetyPreservesContextError(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "canceled", true: "deadline"}[expired], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			want := context.Canceled
			if expired {
				cancel()
				ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
				defer cancel()
				want = context.DeadlineExceeded
			}
			calls := 0
			p := Publisher{readGit: func(context.Context, string, ...string) ([]byte, error) {
				calls++
				cancel()
				return nil, ctx.Err()
			}}
			err := p.checkPublication(ctx, Target{Base: "main"}, strings.Repeat("b", 40))
			if !errors.Is(err, want) || calls != 1 {
				t.Fatalf("publication error=%v reads=%d, want %v without fallback", err, calls, want)
			}
		})
	}
}

func publicationSafetyFixture(t *testing.T, baseRef, tree, blob string) func(context.Context, string, ...string) ([]byte, error) {
	t.Helper()
	base, head, object := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	return func(ctx context.Context, _ string, args ...string) ([]byte, error) {
		t.Helper()
		if _, ok := ctx.Deadline(); !ok {
			t.Error("publication inspection has no deadline")
		}
		switch args[0] {
		case "rev-parse":
			if slices.Equal(args, []string{"rev-parse", "--is-shallow-repository"}) {
				return []byte("false\n"), nil
			}
			if !slices.Equal(args[:3], []string{"rev-parse", "--verify", "--end-of-options"}) {
				t.Fatal("unsafe revision inspection", args)
			}
			switch args[3] {
			case baseRef + "^{commit}", base + "^{commit}":
				return []byte(base + "\n"), nil
			case head + "^{commit}":
				return []byte(head + "\n"), nil
			default:
				return nil, errors.New("missing revision")
			}
		case "rev-list":
			if !slices.Equal(args, []string{"rev-list", head, "^" + base, "--"}) {
				t.Fatal("publication did not scan exact tip against target base", args)
			}
			return []byte(head + "\n"), nil
		case "diff-tree":
			if !slices.Equal(args, []string{"diff-tree", "--root", "-r", "-m", "--no-commit-id", "--no-renames", "--raw", "-z", "--diff-filter=ACMT", head, "--"}) {
				t.Fatal("unexpected tree inspection", args)
			}
			return []byte(tree), nil
		case "cat-file":
			if !slices.Equal(args, []string{"cat-file", "blob", object}) {
				t.Fatal("unexpected blob inspection", args)
			}
			return []byte(blob), nil
		default:
			t.Fatal("unexpected local Git inspection", args)
			return nil, errors.New("unexpected inspection")
		}
	}
}

func TestPublicationSafetyTargetBaseResolution(t *testing.T) {
	for _, baseRef := range []string{"refs/remotes/origin/main", "refs/heads/main", "missing"} {
		t.Run(baseRef, func(t *testing.T) {
			p := Publisher{readGit: publicationSafetyFixture(t, baseRef, "", "")}
			err := p.checkPublication(t.Context(), Target{Base: "main"}, strings.Repeat("b", 40))
			if (err != nil) != (baseRef == "missing") {
				t.Fatal("wrong base resolution outcome", err)
			}
		})
	}
}

func TestPublicationPRRechecksLegacyConfirmedBranchBeforeEffects(t *testing.T) {
	secret := "ghp_" + strings.Repeat("z", 36)
	for _, scenario := range []struct{ name, path, contents string }{
		{"artifact", ".goobers/debug.log", "private diagnostics"},
		{"credential", "notes.txt", secret},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			q, target, _ := publicationFixture(t)
			sha := confirmedPublicationBranch(t, q, target)
			fake := &publicationHTTP{t: t, target: target}
			tree := ":000000 100644 " + strings.Repeat("0", 40) + " " + strings.Repeat("c", 40) + " A\x00" + scenario.path + "\x00"
			p := Publisher{
				Queue: q, Git: publicationGitFake{sha: sha},
				PRs:     providers.NewGitHubProvider("token", providers.WithHTTPClient(fake), providers.WithMaxTransientRetries(0)),
				readGit: publicationSafetyFixture(t, "refs/remotes/origin/main", tree, scenario.contents),
			}
			if _, err := p.OpenPR(t.Context(), target, "child title", "body", true); err == nil || !strings.Contains(err.Error(), "publication safety") || strings.Contains(err.Error(), secret) {
				t.Fatal("unsafe confirmed branch admitted or diagnostics leaked", err)
			}
			if fake.creates != 0 || fake.patches != 0 {
				t.Fatal("unsafe confirmed branch reached provider writes")
			}
			if _, err := q.ChildPublication(t.Context(), target.Child.Identity, "pr"); !errors.Is(err, triggerqueue.ErrChildPublicationPending) {
				t.Fatal("unsafe branch prepared a PR intent", err)
			}
		})
	}
}

func TestPublicationReadBufferBounds(t *testing.T) {
	var out publicationReadBuffer
	chunk := bytes.Repeat([]byte{'x'}, 64<<10)
	for range publicationReadLimit / len(chunk) {
		if _, err := out.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	// os/exec copies from an ordinary reader. An embedded bytes.Buffer would
	// promote ReaderFrom and let io.Copy bypass the bounded Write method.
	if _, err := io.Copy(&out, io.LimitReader(strings.NewReader("x"), 1)); err == nil || out.Len() != publicationReadLimit {
		t.Fatal("publication read did not enforce its bound", err)
	}
	var transport publicationOutput
	if _, err := io.Copy(&transport, io.LimitReader(bytes.NewReader(chunk), int64(len(chunk)))); err == nil || transport.Len() > 16<<10 {
		t.Fatal("publication transport did not enforce its bound", err)
	}
}
