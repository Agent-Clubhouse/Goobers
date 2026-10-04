package triggerqueue

import (
	"crypto/sha1"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/providers"
)

func TestWorkbenchProposalIdentityOperationsSurviveReopenAndPinExactRequest(t *testing.T) {
	for _, operation := range []string{"objective", "alias"} {
		t.Run(operation, func(t *testing.T) {
			input, plan := proposalFixture(t, providers.ProviderGitHub, "identity")
			input.Request.Field, input.Request.Value = "", nil
			if operation == "objective" {
				input.Request.Objective = &workbench.MetadataObjectiveAssignment{ObjectiveID: "obj-11111111-1111-1111-1111-111111111111", Title: "Assigned objective"}
			} else {
				input.Request.Path = "relationships.yaml"
				plan.Kind = "relationships"
				plan.Native.Path = input.Request.Path
				plan.Preview.Before = "schemaVersion: relationships/v1\nedges: []\n"
				input.Request.Alias = &workbench.MetadataAliasEdit{Action: "add", Alias: workbench.Alias{Name: "objective", Target: workbench.NodeRef{GaggleID: input.Scope.Gaggle, SourceBindingID: input.Scope.SourceBindingID, Kind: "objective-document", SourceID: "obj-11111111-1111-1111-1111-111111111111"}}}
			}
			raw := []byte(plan.Preview.Before)
			blob := sha1.Sum(append(fmt.Appendf(nil, "blob %d\x00", len(raw)), raw...))
			input.Request.Expected.BlobID = fmt.Sprintf("%x", blob)
			input.Request.Expected.ContentDigest = childDigest(raw)
			plan.Native.PreviousBlob = input.Request.Expected.BlobID
			set, err := proposalValidationSources(input, plan)
			if err != nil {
				t.Fatal(err)
			}
			file := workbench.MetadataFile{Path: input.Request.Path, Content: raw, Provenance: workbench.SourceProvenance{Commit: input.Request.Expected.Commit, BlobID: input.Request.Expected.BlobID, ContentDigest: input.Request.Expected.ContentDigest}}
			plan.Preview, err = workbench.PreviewMetadataChange(set, input.Scope.SourceBindingID, file, input.Request)
			if err != nil {
				t.Fatal(err)
			}
			input.TargetDigest, input.OperationDigest = plan.Preview.TargetDigest, plan.Preview.OperationDigest
			plan.Native.OperationDigest = input.OperationDigest
			plan.Native.Content = []byte(plan.Preview.After)
			path := filepath.Join(t.TempDir(), "queue.db")
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			record, _, err := store.AcceptWorkbenchProposal(t.Context(), input, childTestTime)
			if err != nil {
				t.Fatal(err)
			}
			plan.Native.CommandID = record.ID[10:]
			marker := providers.RepositoryProposalMarker(plan.Native.CommandID, plan.Native.OperationDigest)
			plan.Native.Message = "Assign metadata\n\n" + marker
			plan.Native.Body = "Human requested metadata.\n\n" + marker
			if _, err = store.AttachWorkbenchProposalPlan(t.Context(), input.Scope, record.ID, record.RequestDigest, plan); err != nil {
				t.Fatal(err)
			}
			if err = store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			retained, err := store.WorkbenchProposal(t.Context(), input.Scope, record.ID)
			if err != nil || retained.Plan.Preview.After != plan.Preview.After || retained.State != "prepared" {
				t.Fatal(retained, err)
			}
			if input.Request.Objective != nil {
				input.Request.Objective.Title = "Changed after acceptance"
			} else {
				input.Request.Alias.Alias.Name = "different"
			}
			if _, _, err := store.AcceptWorkbenchProposal(t.Context(), input, childTestTime); err == nil {
				t.Fatal("changed request replay accepted")
			}
		})
	}
}
