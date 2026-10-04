package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/internal/workbenchprovider"
	"github.com/goobers/goobers/internal/workbenchservice"
	"github.com/goobers/goobers/providers"
)

type hostProposalClient struct {
	*hostDocumentReader
	phases []string
	lost   bool
}

func (c *hostProposalClient) ApplyRepositoryProposalPhase(_ context.Context, in providers.RepositoryProposalPhaseInput) (providers.RepositoryProposalPhaseResult, error) {
	c.phases = append(c.phases, in.Phase)
	if in.Proposal.Repository.Owner != c.target.Owner || in.Proposal.Repository.Name != c.target.Name || in.Proposal.Path != "plan.md" || in.Proposal.BaseBranch != "strategy" || string(in.Proposal.Content) != "# Revised strategy\n" {
		c.t.Fatal("proposal escaped reviewed source", in)
	}
	result := providers.RepositoryProposalPhaseResult{MutationAttempted: true, Acknowledged: true}
	switch in.Phase {
	case "tree":
		result.TreeID = strings.Repeat("b", 40)
	case "commit":
		result.TreeID, result.CommitID = strings.Repeat("b", 40), strings.Repeat("c", 40)
	case "branch":
		result.CommitID = strings.Repeat("c", 40)
	case "pull-request":
		if c.lost {
			return providers.RepositoryProposalPhaseResult{MutationAttempted: true}, io.ErrUnexpectedEOF
		}
		result.CommitID = strings.Repeat("c", 40)
		result.PullRequest = &providers.PullRequestResult{ID: "7", Number: 7, URL: "https://github.com/" + c.target.Owner + "/" + c.target.Name + "/pull/7"}
	default:
		c.t.Fatal("unknown phase", in.Phase)
	}
	return result, nil
}
func (*hostProposalClient) ObserveRepositoryProposalPhase(context.Context, providers.RepositoryProposalPhaseInput) (providers.RepositoryProposalObservation, error) {
	return providers.RepositoryProposalObservation{}, nil
}

func TestWorkbenchHostInstallsGovernedRepositoryProposals(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprint("lost-", lost), func(t *testing.T) {
			setup, pin := interactiveExecutionFixture(t)
			g := setup.Definitions.Gaggles[0].DeepCopy()
			g.Spec.Project.Branch = "strategy"
			target := interactiveRepository(g.Spec.Project)
			g.Spec.Workbench = &apiv1.GaggleWorkbench{SchemaVersion: "sources/v1", Sources: []apiv1.WorkbenchSource{{Name: "strategy", Kind: "documents", Repository: &target, Paths: []string{"plan.md"}, Writes: &apiv1.WorkbenchWrites{Fields: []apiv1.WorkbenchField{"description"}}}}}
			g.Spec.InteractiveAccess.Actions = append(g.Spec.InteractiveAccess.Actions, "source.proposeChange")
			// Omission retains the mandated PR-only default.
			g.Spec.InteractiveAccess.SourceWrites = nil
			if err := setup.InteractiveAccess.Apply([]apiv1.Gaggle{*g}, nil); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HUMAN_REPO", "proposal-human-canary")
			t.Setenv("GH_TOKEN", "automation-must-not-be-used")
			client := &hostProposalClient{hostDocumentReader: &hostDocumentReader{t: t, target: g.Spec.Project}, lost: lost}
			read := &workbenchservice.Service{Permissions: setup.InteractiveAccess, Repository: func(context.Context, workbenchservice.ReadBinding, interactiveaccess.Credential) (workbenchprovider.RepositoryClient, error) {
				return client, nil
			}}
			factory := func(_ context.Context, binding workbenchservice.ReadBinding, credential interactiveaccess.Credential) (workbenchprovider.RepositoryProposalClient, error) {
				if credential.Value != "proposal-human-canary" || binding.Scope.GaggleID != g.Name || binding.Source.Spec.Name != "strategy" || *binding.Source.Spec.Repository != target {
					t.Fatal("lost exact interactive identity or source")
				}
				return client, nil
			}
			p := httpapi.Principal{Issuer: "issuer", Subject: "human", Roles: []httpapi.Role{httpapi.RoleOperate}}
			available := func() bool {
				v, err := setup.InteractiveAccess.InteractiveCapabilities(t.Context(), p, g.Name)
				if err != nil {
					t.Fatal(err)
				}
				for _, action := range v.Actions {
					if action.Action == "source.proposeChange" {
						return action.Available
					}
				}
				t.Fatal("proposal action missing")
				return false
			}
			u := &upSession{}
			u.setup, u.l = setup, pin.layout
			u.installWorkbenchProposals(read, factory)
			if available() {
				t.Fatal("proposal advertised without durable queue")
			}
			u.durableTriggers = acceptedService(t, filepath.Join(pin.layout.SchedulerDir(), "accepted-triggers.db"), newDaemonTriggerService())
			u.installWorkbenchProposals(read, factory)
			if !available() {
				t.Fatal("installed configured proposal unavailable")
			}
			opts := append(u.apiHandlerOpts, httpapi.WithAuthenticator(interactiveTestAuthenticator{principal: &p}))
			handler, err := httpapi.NewHandler(&readservice.Local{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), opts...)
			if err != nil {
				t.Fatal(err)
			}
			body := "# Revised strategy\n"
			request := workbench.MetadataChangeRequest{Path: "plan.md", Expected: workbench.MetadataRevision{Commit: strings.Repeat("a", 40), BlobID: "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391", ContentDigest: fmt.Sprintf("%x", sha256.Sum256(nil))}, Field: "description", Value: &body}
			raw, _ := json.Marshal(request)
			base := "/api/v1/gaggles/" + g.Name + "/workbench/sources/strategy/"
			call := func(method, route, key string, input []byte) *httptest.ResponseRecorder {
				r := httptest.NewRequest(method, base+route, strings.NewReader(string(input)))
				r.Header.Set("Content-Type", "application/json")
				if key != "" {
					r.Header.Set(httpapi.HeaderIdempotencyKey, key)
				}
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				return w
			}
			preview := call(http.MethodPost, "proposal-preview", "", raw)
			if preview.Code != 200 || len(client.phases) != 0 {
				t.Fatal("preview failed or mutated source", preview.Code, preview.Body)
			}
			var command workbench.MetadataProposalCommand
			for range 2 {
				response := call(http.MethodPost, "proposals", "same-request", raw)
				if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &command) != nil {
					t.Fatal(response.Code, response.Body)
				}
				state := "confirmed"
				if lost {
					state = "unknown"
				}
				if command.State != state || len(client.phases) != 4 || command.Actor.Subject != p.Subject {
					t.Fatal("proposal custody or replay differed", command, client.phases)
				}
			}
			g.Spec.Workbench.Sources[0].Writes = nil
			if err = setup.InteractiveAccess.Apply([]apiv1.Gaggle{*g}, nil); err != nil {
				t.Fatal(err)
			}
			if available() {
				t.Fatal("revoked source edit still advertised")
			}
			if denied := call(http.MethodPost, "proposal-preview", "", raw); denied.Code != 403 {
				t.Fatal("revoked proposal reached source", denied.Code, denied.Body)
			}
			if receipt := call(http.MethodGet, "proposals/"+command.ID, "", nil); receipt.Code != 200 {
				t.Fatal("read authority lost retained receipt", receipt.Code, receipt.Body)
			}
			if len(client.phases) != 4 {
				t.Fatal("revocation or receipt check repeated mutation")
			}
		})
	}
}
