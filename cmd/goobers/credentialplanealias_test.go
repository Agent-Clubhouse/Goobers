package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
)

func TestCredentialPlaneLegacyRuntimeAliasResolvesOnce(t *testing.T) {
	service, _, runID := newCredentialPlaneFixture(t, compileCredentialPlaneMachine(t, credentialPlaneSpec()))
	// Use the actual single-gaggle migration, including its native directory
	// junction on Windows, rather than manufacturing a duplicate run record.
	if err := service.layout.MigrateLegacyRuntime([]string{"web"}); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(service.layout.ForGaggle("web").RunsDir(), runID)
	got, err := service.locateRun(*service.defs.Load(), runID)
	if err != nil || got != want {
		t.Fatalf("locate migrated run = %q, %v; want scoped path %q", got, err, want)
	}
	resolved, err := service.Resolve(context.Background(), httpapi.CredentialResolveRequest{
		RunID: runID, Stage: "implement", Capabilities: []string{"repo:push"},
	})
	if err != nil {
		t.Fatalf("resolve stage on migrated run: %v", err)
	}
	if len(resolved.Credentials) != 1 || resolved.Credentials[0].Capability != "repo:push" {
		t.Fatal("migrated run did not resolve its declared stage capability")
	}
}

func TestCredentialPlaneRunAliasesPreserveAmbiguity(t *testing.T) {
	for _, kind := range []string{"other-gaggle", "legacy-copy", "legacy-hard-linked-metadata", "other-gaggle-alias"} {
		t.Run(kind, func(t *testing.T) {
			machine := compileCredentialPlaneMachine(t, credentialPlaneSpec())
			service, _, runID := newCredentialPlaneFixture(t, machine)
			scoped := service.layout.ForGaggle("web").RunsDir()
			other := service.layout.RunsDir()
			if kind == "other-gaggle" || kind == "other-gaggle-alias" {
				other = service.layout.ForGaggle("second").RunsDir()
				defs := *service.defs.Load()
				defs.Scopes = map[string]credentialGaggleScope{"web": {}, "second": {}}
				service.Replace(defs)
			}
			if kind == "other-gaggle-alias" {
				if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := instance.CreateLegacyRuntimeAlias(other, scoped); err != nil {
					t.Fatal(err)
				}
			} else {
				// Even identical or hard-linked run.yaml files in distinct run
				// directories cannot establish one journal/inputs ownership.
				if err := os.MkdirAll(filepath.Join(other, runID), 0o755); err != nil {
					t.Fatal(err)
				}
				source := filepath.Join(scoped, runID, "run.yaml")
				target := filepath.Join(other, runID, "run.yaml")
				if kind == "legacy-hard-linked-metadata" {
					if err := os.Link(source, target); err != nil {
						t.Fatal(err)
					}
				} else {
					data, err := os.ReadFile(source)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(target, data, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			_, err := service.Resolve(context.Background(), httpapi.CredentialResolveRequest{
				RunID: runID, Stage: "implement", Capabilities: []string{"repo:push"},
			})
			planeErr := planeErrorOf(t, err)
			if planeErr.Status != http.StatusConflict || planeErr.Code != "ambiguous_run_id" {
				t.Fatalf("duplicate ownership refusal = %d %s", planeErr.Status, planeErr.Code)
			}
		})
	}
}
