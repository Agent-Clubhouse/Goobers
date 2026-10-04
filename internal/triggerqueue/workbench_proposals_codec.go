package triggerqueue

import (
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/providers"
)

const workbenchProposalColumns = "id,gaggle,source_binding,issuer,subject,request_id,request_digest,target_digest,operation_digest,request,state,accepted_ns,completed_ns,tombstoned_ns,plan,plan_digest,before_source,after_source,history,history_digest"

type workbenchProposalHistory struct {
	Phases              []WorkbenchProposalPhase
	Observations        []WorkbenchProposalObservation
	OmittedObservations int64
}

func canonicalWorkbenchProposal(input WorkbenchProposalInput) ([]byte, string, error) {
	if !validWorkbenchScope(input.Scope) || !validChildText(input.RequestID, 256, true) || !validWorkbenchDigest(input.TargetDigest) || !validWorkbenchDigest(input.OperationDigest) || !validMetadataRequest(input.Scope, input.Request) {
		return nil, "", ErrTransition
	}
	raw, err := json.Marshal(input.Request)
	if err != nil || len(raw) > MaxWorkbenchProposalRequestBytes {
		return nil, "", ErrTransition
	}
	return raw, childDigest(raw), nil
}

func validMetadataRequest(scope WorkbenchCommandScope, r workbench.MetadataChangeRequest) bool {
	bindings := map[string]bool{scope.SourceBindingID: true}
	if r.Relationship != nil {
		bindings[r.Relationship.Edge.From.SourceBindingID] = true
		bindings[r.Relationship.Edge.To.SourceBindingID] = true
	}
	if r.Alias != nil {
		bindings[r.Alias.Alias.Target.SourceBindingID] = true
	}
	return workbench.ValidateMetadataChangeRequest(workbench.Scope{GaggleID: scope.Gaggle, Bindings: bindings}, scope.SourceBindingID, r) == nil
}

func scanWorkbenchProposal(row scanner) (WorkbenchProposal, error) {
	var r WorkbenchProposal
	var request, plan, before, after, history []byte
	var accepted int64
	var completed, tombstoned sql.NullInt64
	i := &r.Input
	err := row.Scan(&r.ID, &i.Scope.Gaggle, &i.Scope.SourceBindingID, &i.Scope.Actor.Issuer, &i.Scope.Actor.Subject, &i.RequestID, &r.RequestDigest, &i.TargetDigest, &i.OperationDigest, &request, &r.State, &accepted, &completed, &tombstoned, &plan, &r.PlanDigest, &before, &after, &history, &r.HistoryDigest)
	if err != nil {
		return r, err
	}
	r.AcceptedAt = time.Unix(0, accepted).UTC()
	r.CompletedAt, r.TombstonedAt = workbenchTime(completed), workbenchTime(tombstoned)
	if !validWorkbenchScope(i.Scope) || !validWorkbenchID(r.ID) || !validWorkbenchDigest(r.RequestDigest) || !validWorkbenchDigest(i.TargetDigest) || !validWorkbenchDigest(i.OperationDigest) {
		return r, ErrTransition
	}
	if r.TombstonedAt != nil {
		if r.State != "tombstoned" || r.CompletedAt == nil || r.TombstonedAt.Before(*r.CompletedAt) || len(request)+len(plan)+len(history)+len(before)+len(after) != 0 {
			return r, ErrTransition
		}
		return r, nil
	}
	if len(request) > MaxWorkbenchProposalRequestBytes || childDigest(request) != r.RequestDigest || json.Unmarshal(request, &i.Request) != nil || !validMetadataRequest(i.Scope, i.Request) {
		return r, errors.New("workbench proposal request custody differs")
	}
	if err := decodeProposalCustody(&r, plan, before, after, history); err != nil {
		return r, err
	}
	return r, validateWorkbenchProposalState(r)
}

func decodeProposalCustody(r *WorkbenchProposal, plan, before, after, history []byte) error {
	if len(plan) > 0 {
		r.Plan = &WorkbenchProposalPlan{}
		if len(plan) > MaxWorkbenchProposalPlanBytes || json.Unmarshal(plan, r.Plan) != nil {
			return ErrTransition
		}
		r.Plan.Preview.Before, r.Plan.Preview.After = string(before), string(after)
		r.Plan.Native.Content = append([]byte(nil), after...)
		if proposalPlanDigest(plan, before, after) != r.PlanDigest || validateWorkbenchProposalPlan(*r, *r.Plan) != nil {
			return errors.New("workbench proposal source custody differs")
		}
	} else if r.PlanDigest != "" || len(before)+len(after) > 0 {
		return ErrTransition
	}
	if len(history) > 0 {
		var h workbenchProposalHistory
		if len(history) > MaxWorkbenchProposalHistoryBytes || childDigest(history) != r.HistoryDigest || json.Unmarshal(history, &h) != nil {
			return ErrTransition
		}
		r.Phases, r.Observations, r.OmittedObservations = h.Phases, h.Observations, h.OmittedObservations
	} else if r.HistoryDigest != "" {
		return ErrTransition
	}
	return nil
}

func canonicalProposalPlan(plan WorkbenchProposalPlan) (header, before, after []byte, digest string, err error) {
	before, after = []byte(plan.Preview.Before), []byte(plan.Preview.After)
	plan.Preview.Before, plan.Preview.After = "", ""
	plan.Native.Content = nil
	header, err = json.Marshal(plan)
	if err != nil || len(header) > MaxWorkbenchProposalPlanBytes || len(before) > workbench.MaxSourceBytes || len(after) > workbench.MaxSourceBytes {
		return nil, nil, nil, "", ErrTransition
	}
	return header, before, after, proposalPlanDigest(header, before, after), nil
}

func proposalPlanDigest(header, before, after []byte) string {
	// Fixed-width hashes make the concatenation unambiguous without copying
	// both source files again into an expanded JSON string representation.
	return childDigest([]byte(childDigest(header) + childDigest(before) + childDigest(after)))
}

func validateWorkbenchProposalPlan(record WorkbenchProposal, plan WorkbenchProposalPlan) error {
	r := record.Input.Request
	n := plan.Native
	if plan.Scope.GaggleID != record.Input.Scope.Gaggle || !plan.Scope.Bindings[record.Input.Scope.SourceBindingID] || plan.Scope.Validate() != nil || n.CommandID != record.ID[10:] || n.OperationDigest != record.Input.OperationDigest || n.BaseCommit != r.Expected.Commit || n.PreviousBlob != r.Expected.BlobID || n.Path != r.Path || string(n.Content) != plan.Preview.After || providers.ValidateRepositoryProposal(n) != nil {
		return ErrTransition
	}
	file := providers.RepositorySourceFile{Path: r.Path, Commit: r.Expected.Commit, BlobID: r.Expected.BlobID, Content: []byte(plan.Preview.Before)}
	if providers.ValidateRepositorySourceFile(file, r.Path, r.Expected.Commit) != nil {
		return ErrTransition
	}
	set, err := proposalValidationSources(record.Input, plan)
	if err != nil {
		return err
	}
	current := workbench.MetadataFile{Path: r.Path, Content: file.Content, Provenance: workbench.SourceProvenance{Commit: r.Expected.Commit, BlobID: r.Expected.BlobID, ContentDigest: r.Expected.ContentDigest}}
	preview, err := workbench.PreviewMetadataChange(set, record.Input.Scope.SourceBindingID, current, r)
	if err != nil || !preview.Changed || !reflect.DeepEqual(preview, plan.Preview) || preview.TargetDigest != record.Input.TargetDigest || preview.OperationDigest != record.Input.OperationDigest {
		return ErrTransition
	}
	return nil
}

// Reconstructed declarations validate retained syntax/content, not authority.
// Live source permissions are independently checked by the service every time.
func proposalValidationSources(input WorkbenchProposalInput, plan WorkbenchProposalPlan) (workbench.SourceSet, error) {
	if (plan.Kind != "documents" && plan.Kind != "relationships") || (plan.Kind == "documents" && !strings.HasSuffix(strings.ToLower(input.Request.Path), ".md")) {
		return workbench.SourceSet{}, ErrTransition
	}
	n := plan.Native.Repository
	repository := apiv1.InteractiveRepositoryIdentity{Provider: apiv1.Provider(n.Provider), Owner: n.Owner, Project: n.Project, Name: n.Name}
	source := workbench.BoundSource{Spec: apiv1.WorkbenchSource{Name: input.Scope.SourceBindingID, Kind: plan.Kind, Repository: &repository, Paths: []string{input.Request.Path}, Writes: &apiv1.WorkbenchWrites{}}, Repository: apiv1.RepoRef{Provider: repository.Provider, Owner: n.Owner, Project: n.Project, Name: n.Name, Branch: plan.Native.BaseBranch}}
	if input.Request.Objective != nil {
		source.Spec.Writes.Metadata = []apiv1.WorkbenchMetadataOperation{"assign-objective"}
	} else if input.Request.Alias != nil {
		source.Spec.Writes.Metadata = []apiv1.WorkbenchMetadataOperation{"aliases"}
	} else if input.Request.Relationship != nil {
		source.Spec.Writes.Relationships = []apiv1.WorkbenchRelationship{apiv1.WorkbenchRelationship(input.Request.Relationship.Edge.Kind)}
	} else {
		source.Spec.Writes.Fields = []apiv1.WorkbenchField{input.Request.Field}
	}
	set := workbench.SourceSet{Scope: plan.Scope, Sources: []workbench.BoundSource{source}}
	if plan.Kind == "relationships" {
		if !strings.HasSuffix(strings.ToLower(input.Request.Path), ".yaml") && !strings.HasSuffix(strings.ToLower(input.Request.Path), ".yml") {
			return workbench.SourceSet{}, ErrTransition
		}
		set.ManifestOwner = &workbench.Owner{Kind: "manifest", SourceBindingID: source.Spec.Name, Path: input.Request.Path}
		if input.Request.Relationship != nil && input.Request.Relationship.Edge.From.SourceBindingID != source.Spec.Name {
			set.Sources = append(set.Sources, workbench.BoundSource{Spec: apiv1.WorkbenchSource{Name: input.Request.Relationship.Edge.From.SourceBindingID, Kind: "backlog"}})
		}
	}
	return set, nil
}
