package apireadcache

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
)

func TestSnapshotInvalidationProcess(t *testing.T) {
	if os.Getenv("GOOBERS_TEST_INVALIDATE_SNAPSHOT") != "1" {
		return
	}
	scope := Scope{Gaggle: "team", Binding: "automation", Generation: "generation"}
	if err := InvalidateScopedSnapshot(os.Getenv("GOOBERS_TEST_CACHE_DIRECTORY"), "tick", scope); err != nil {
		t.Fatal(err)
	}
}

func TestMutationInvalidationProcess(t *testing.T) {
	if os.Getenv("GOOBERS_TEST_MUTATE_PROVIDER") != "1" {
		return
	}
	scope := Scope{Gaggle: "team", Binding: "interactive", Generation: "new-generation"}
	kind := providers.ProviderKind(os.Getenv("GOOBERS_TEST_MUTATION_PROVIDER"))
	inner := scopedTransport(func(*http.Request) (*http.Response, error) {
		return mutationResponse(201, `{"id":81,"text":"prepared","body":"prepared"}`), nil
	})
	client := ScopedClient(os.Getenv("GOOBERS_TEST_CACHE_DIRECTORY"), "tick", scope, kind, inner)
	if kind == providers.ProviderGitHub {
		writeMutationComment(t, client)
		return
	}
	p := providers.NewADOProvider("org", "project", "token", func(p *providers.ADOProvider) { p.Client = client })
	if _, err := p.CreateWorkItemComment(t.Context(), providers.RepositoryRef{Project: "project"}, "1", "prepared"); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotLateFillCannotReviveInvalidationFromAnotherProcess(t *testing.T) {
	testLateFillAfterIndependentInvalidation(t, false)
}

func TestProviderMutationFromAnotherProcessFencesLateGETAndADOPlanFills(t *testing.T) {
	testLateFillAfterIndependentInvalidation(t, true)
}

func testLateFillAfterIndependentInvalidation(t *testing.T, mutation bool) {
	t.Helper()
	for _, provider := range []providers.ProviderKind{providers.ProviderGitHub, providers.ProviderADO} {
		t.Run(string(provider), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			dir := t.TempDir()
			scope := Scope{Gaggle: "team", Binding: "automation", Generation: "generation"}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			var calls atomic.Int32
			transport := scopedTransport(func(req *http.Request) (*http.Response, error) {
				n := calls.Add(1)
				if n == 1 {
					close(entered)
					select {
					case <-release:
					case <-req.Context().Done():
						return nil, req.Context().Err()
					}
				}
				body := fmt.Sprintf(`[{"number":%d}]`, n)
				if provider == providers.ProviderADO {
					body = `{"workItems":[]}`
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			})
			client := ScopedClient(dir, "tick", scope, provider, transport)
			read := func() error {
				if provider == providers.ProviderADO {
					p := providers.NewADOProvider("org", "project", "token", func(p *providers.ADOProvider) { p.Client = client })
					_, err := p.ListWorkItems(ctx, providers.ListWorkItemsRequest{Repository: providers.RepositoryRef{Project: "project"}, State: "all"})
					return err
				}
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/org/repo/issues", nil)
				if err != nil {
					return err
				}
				req.Header.Set("Authorization", "Bearer token")
				resp, err := client.Do(req)
				if err != nil {
					return err
				}
				defer func() { _ = resp.Body.Close() }()
				_, err = io.Copy(io.Discard, resp.Body)
				return err
			}
			result := make(chan error, 1)
			go func() { result <- read() }()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSnapshotInvalidationProcess$")
			command.Env = append(os.Environ(), "GOOBERS_TEST_INVALIDATE_SNAPSHOT=1", "GOOBERS_TEST_CACHE_DIRECTORY="+dir)
			if mutation {
				command = exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMutationInvalidationProcess$")
				command.Env = append(os.Environ(), "GOOBERS_TEST_MUTATE_PROVIDER=1", "GOOBERS_TEST_MUTATION_PROVIDER="+string(provider), "GOOBERS_TEST_CACHE_DIRECTORY="+dir)
			}
			if out, err := command.CombinedOutput(); err != nil {
				t.Fatalf("independent invalidation: %v: %s", err, out)
			}
			unblock()
			if err := <-result; err != nil {
				t.Fatal(err)
			}
			// The original response may complete, but cannot refill the active snapshot.
			if err := read(); err != nil {
				t.Fatal(err)
			}
			if err := read(); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 2 {
				t.Fatalf("late fill revived stale snapshot or fresh fill failed to share: %d calls", calls.Load())
			}
		})
	}
}
