package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

func TestConnectADOSeedCLI(t *testing.T) {
	t.Setenv("GOOBERS_ADO_TOKEN", "test-pat")
	stubConnectReachability(t, nil)
	root := filepath.Join(t.TempDir(), "ado")
	code, _, stderr := runArgs(t, "init", "--template=standard", "--provider=ado", "--ci-command=[\"dotnet\",\"test\"]", "--required-capabilities=dotnet@8", root)
	if code != 0 {
		t.Fatalf("init: %d %s", code, stderr)
	}
	client := &fakeADOSeedClient{}
	client.list = func(req providers.ListWorkItemsRequest) ([]providers.WorkItem, error) {
		if len(client.created) == 0 {
			return nil, nil
		}
		return []providers.WorkItem{{Body: "goobers run-id: " + client.created[0].RunID}}, nil
	}
	previous := newConnectADOSeeder
	newConnectADOSeeder = func(repo providers.RepositoryRef, token string) connectADOSeeder {
		if repo.Provider != providers.ProviderADO || repo.Owner != "contoso" || repo.Project != "boards" || repo.Name != "web" || token != "test-pat" {
			t.Fatalf("incorrect ADO seed target or credential: %+v", repo)
		}
		return client
	}
	t.Cleanup(func() { newConnectADOSeeder = previous })
	forbidGuidedADOClient(t)
	for attempt := range 2 {
		code, stdout, stderr := runArgs(t, "connect", "contoso/boards/web", "--token-env", "GOOBERS_ADO_TOKEN", "--seed", "--json", root)
		if code != 0 {
			t.Fatalf("seed attempt %d: %d %s", attempt, code, stderr)
		}
		result := connectEnvelope(t, stdout)
		if !strings.Contains(stderr, "1 open work items match selector tags") || strings.Contains(stdout, "note:") {
			t.Fatalf("ADO reality advisory missing or corrupted JSON: stdout=%s stderr=%s", stdout, stderr)
		}
		if attempt == 0 && !slices.Contains(result.Created, "issue:hello-goobers") {
			t.Fatalf("seed not reported: %+v", result)
		}
		if attempt == 1 && !slices.Contains(result.Skipped, "issue:hello-goobers") {
			t.Fatalf("repeat not reported: %+v", result)
		}
	}
	if len(client.created) != 1 || len(client.created[0].Labels) == 0 || slices.Contains(client.created[0].Labels, providers.LabelClaimed) {
		t.Fatalf("incorrect starter task: %+v", client.created)
	}
}

// forbidGuidedADOClient fails the test if anything builds an Azure DevOps
// client from the configured credential source, which on a developer host
// could reach the real service through a live Azure CLI login.
func forbidGuidedADOClient(t *testing.T) {
	t.Helper()
	previous := newGuidedADOClient
	newGuidedADOClient = func(repo instance.RepoRef, _ credentials.StoreResolver, _ providers.SecretRegistrar) (connectADOSeeder, error) {
		t.Fatalf("unexpected configured-source ADO client for %+v", repo)
		return nil, nil
	}
	t.Cleanup(func() { newGuidedADOClient = previous })
}

// With a kept non-PAT source, --seed files through that source (the client
// the guided flow uses), never through the token variable the instance does
// not name, and the token-based selector echo does not run.
func TestConnectADOSeedThroughKeptAuth(t *testing.T) {
	t.Setenv("GOOBERS_ADO_TOKEN", "test-pat")
	stubConnectReachability(t, errors.New("token preflight must not run"))
	root := filepath.Join(t.TempDir(), "ado")
	code, _, stderr := runArgs(t, "init", "--template=standard", "--provider=ado", "--ci-command=[\"dotnet\",\"test\"]", "--required-capabilities=dotnet@8", root)
	if code != 0 {
		t.Fatalf("init: %d %s", code, stderr)
	}
	previousSeeder := newConnectADOSeeder
	newConnectADOSeeder = func(providers.RepositoryRef, string) connectADOSeeder {
		t.Fatal("seeded through the token variable instead of the kept auth source")
		return nil
	}
	t.Cleanup(func() { newConnectADOSeeder = previousSeeder })
	client := &fakeADOSeedClient{list: func(providers.ListWorkItemsRequest) ([]providers.WorkItem, error) { return nil, nil }}
	previous := newGuidedADOClient
	newGuidedADOClient = func(repo instance.RepoRef, _ credentials.StoreResolver, registrar providers.SecretRegistrar) (connectADOSeeder, error) {
		if repo.Owner != "contoso" || repo.Project != "boards" || repo.Name != "web" || repo.Auth == nil ||
			repo.Auth.Kind != instance.ADOAuthAzureCLI || registrar == nil {
			t.Fatalf("seed client built from %+v auth=%+v", repo, repo.Auth)
		}
		return client, nil
	}
	t.Cleanup(func() { newGuidedADOClient = previous })
	code, stdout, stderr := runArgs(t, "connect", "contoso/boards/web", "--seed", "--json", root)
	if code != 0 {
		t.Fatalf("connect --seed: %d %s", code, stderr)
	}
	result := connectEnvelope(t, stdout)
	if !slices.Contains(result.Created, "issue:hello-goobers") || len(client.created) != 1 {
		t.Fatalf("seed not filed through the kept source: %+v created=%+v", result, client.created)
	}
	if strings.Contains(stderr, "match selector tags") {
		t.Fatalf("token-based selector echo ran with a kept auth source: %s", stderr)
	}
}

type fakeADOSeedClient struct {
	list      func(providers.ListWorkItemsRequest) ([]providers.WorkItem, error)
	created   []providers.CreateWorkItemRequest
	createErr error
}

func (f *fakeADOSeedClient) ListWorkItems(_ context.Context, req providers.ListWorkItemsRequest) ([]providers.WorkItem, error) {
	return f.list(req)
}

func (f *fakeADOSeedClient) CreateWorkItem(_ context.Context, req providers.CreateWorkItemRequest) (providers.WorkItem, error) {
	f.created = append(f.created, req)
	return providers.WorkItem{}, f.createErr
}

func TestADOSeedStarterTagsAndRepeat(t *testing.T) {
	catalog := connectSeedCatalog([]string{"ready"}, []string{"claimed"})
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "boards", Name: "web"}
	client := &fakeADOSeedClient{}
	client.list = func(req providers.ListWorkItemsRequest) ([]providers.WorkItem, error) {
		if req.Repository != repo || req.State != "all" || req.Limit != 100 || req.PageInfo == nil {
			t.Fatalf("incorrect scan: %+v", req)
		}
		if len(client.created) == 0 {
			return nil, nil
		}
		return []providers.WorkItem{{Body: "goobers run-id: " + client.created[0].RunID}}, nil
	}
	var first, repeat onboardingActionResult
	if err := seedADOStarter(context.Background(), client, repo, catalog, &first); err != nil {
		t.Fatal(err)
	}
	if err := seedADOStarter(context.Background(), client, repo, catalog, &repeat); err != nil {
		t.Fatal(err)
	}
	if len(client.created) != 1 || client.created[0].Repository != repo || client.created[0].Type != "Task" || !slices.Equal(client.created[0].Labels, []string{"ready"}) {
		t.Fatalf("incorrect creation: %+v", client.created)
	}
	if !slices.Equal(first.Created, []string{"issue:hello-goobers"}) || !slices.Equal(repeat.Skipped, first.Created) {
		t.Fatalf("incorrect results: first=%+v repeat=%+v", first, repeat)
	}
}

func TestADOSeedIdentitySeparatesRepositories(t *testing.T) {
	first := connectADOSeedCatalog(connectOptions{ado: &connectADORepo{Organization: "org", Project: "one", Repository: "web"}}, nil)
	second := connectADOSeedCatalog(connectOptions{ado: &connectADORepo{Organization: "org", Project: "two", Repository: "web"}}, nil)
	if onboardingSeedRunID(first, first.Issues[0], connectAction) == onboardingSeedRunID(second, second.Issues[0], connectAction) {
		t.Fatal("different repository projects share a seed identity")
	}
}

func TestADOSeedRefusesIncompleteScan(t *testing.T) {
	for _, mode := range []string{"error", "stuck", "limit", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			client := &fakeADOSeedClient{list: func(req providers.ListWorkItemsRequest) ([]providers.WorkItem, error) {
				calls++
				switch mode {
				case "error":
					return nil, errors.New("denied")
				case "stuck":
					req.PageInfo.HasNext = true
				case "limit":
					req.PageInfo.HasNext = true
					req.PageInfo.NextCursor = fmt.Sprint(calls)
				case "oversized":
					return make([]providers.WorkItem, 101), nil
				}
				return nil, nil
			}}
			var result onboardingActionResult
			err := seedADOStarter(context.Background(), client, providers.RepositoryRef{}, connectSeedCatalog(nil, nil), &result)
			if err == nil || len(client.created) != 0 || len(result.Created) != 0 || calls > 10 {
				t.Fatalf("incomplete scan created work: err=%v calls=%d created=%v", err, calls, client.created)
			}
		})
	}
}

func TestADOSeedFindsMarkerOnLaterPage(t *testing.T) {
	catalog := connectSeedCatalog(nil, nil)
	client := &fakeADOSeedClient{list: func(req providers.ListWorkItemsRequest) ([]providers.WorkItem, error) {
		if req.Cursor == "" {
			req.PageInfo.HasNext = true
			req.PageInfo.NextCursor = "100"
			return nil, nil
		}
		return []providers.WorkItem{{Body: "goobers run-id: " + onboardingSeedRunID(catalog, catalog.Issues[0], connectAction)}}, nil
	}}
	var result onboardingActionResult
	if err := seedADOStarter(context.Background(), client, providers.RepositoryRef{}, catalog, &result); err != nil || len(client.created) != 0 || len(result.Skipped) != 1 {
		t.Fatalf("repeat not recognized: %v %+v", err, result)
	}
}
