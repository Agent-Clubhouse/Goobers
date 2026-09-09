package main

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

func TestGuidedADORepositoryPreparation(t *testing.T) {
	for _, mode := range []string{"read-only", "no-create", "create", "incomplete"} {
		t.Run(mode, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "ado")
			code, _, stderr := runArgs(t, "init", "--template=standard", "--provider=ado", "--ci-command=[\"dotnet\",\"test\"]", "--required-capabilities=dotnet@8", root)
			if code != 0 {
				t.Fatalf("init: %d %s", code, stderr)
			}
			set, report, err := instance.LoadConfigDir(filepath.Join(root, "config"))
			if err != nil || report.HasErrors() || len(set.Gaggles) != 1 {
				t.Fatalf("load: %v %+v", err, report)
			}
			gaggle := set.Gaggles[0]
			gaggle.Spec.Backlog.Project = "separate-boards-project"
			client := &fakeADOSeedClient{}
			calls := 0
			client.list = func(req providers.ListWorkItemsRequest) ([]providers.WorkItem, error) {
				calls++
				if req.Repository.Project != "separate-boards-project" {
					t.Fatalf("queried repository project instead of Boards: %+v", req.Repository)
				}
				if mode == "incomplete" {
					req.PageInfo.HasNext = true
					req.PageInfo.NextCursor = fmt.Sprint(calls)
				}
				if len(client.created) > 0 {
					return []providers.WorkItem{{Body: "goobers run-id: " + client.created[0].RunID}}, nil
				}
				return nil, nil
			}
			previous := newGuidedADOClient
			newGuidedADOClient = func(repo instance.RepoRef, stores credentials.StoreResolver, registrar providers.SecretRegistrar) (connectADOSeeder, error) {
				if repo.Project != "your-project" || repo.Auth == nil || repo.Auth.Kind != instance.ADOAuthPAT || repo.Token.Env != "GOOBERS_ADO_TOKEN" || registrar == nil {
					t.Fatalf("configured auth lost: %+v", repo)
				}
				return client, nil
			}
			t.Cleanup(func() { newGuidedADOClient = previous })
			response := guidedRepositoryReadiness{SelectorLabels: []string{"ready"}}
			input := guidedPrepareRepositoryRequest{Apply: mode != "read-only", CreateStarterIssue: mode != "no-create"}
			err = prepareGuidedADORepository(context.Background(), root, gaggle, input, &response)
			if (err != nil) != (mode == "incomplete") {
				t.Fatalf("error=%v mode=%s", err, mode)
			}
			wantCreated := 0
			if mode == "create" {
				wantCreated = 1
			}
			if len(client.created) != wantCreated || response.StarterIssueCreated != (wantCreated == 1) || response.EligibleCount != nil {
				t.Fatalf("unapproved mutation or false eligibility: created=%v response=%+v", client.created, response)
			}
			if mode == "create" && (response.TagMatchCount == nil || *response.TagMatchCount != 1 || !response.TagScanComplete) {
				t.Fatalf("post-create count not refreshed: %+v", response)
			}
		})
	}
}
