package childworkflow

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract/childworkflowwire"
)

// HelperSource compiles bounded shorthand to ordinary one-stage Workflow DSL.
// This initial slice selects the pinned Goober defaults and parent snapshot.
// Nondefault overrides remain refused until their explicit DSL/runtime is wired.
func (v *Validator) HelperSource(key string, request childworkflowwire.HelperRequest) ([]byte, error) {
	if !submissionText(key, childworkflowwire.MaxChildWorkflowInvocationKeyBytes) {
		return nil, ErrSubmissionInvalid
	}
	if err := request.Validate(); err != nil {
		return nil, refusal("helper_input", "", "", err.Error())
	}
	goober, err := v.checkGoober(v.context.Gaggle.Name, request.Goober, "helper", "goober")
	if err != nil {
		return nil, err
	}
	if (request.Harness != "" && request.Harness != string(goober.Harness)) ||
		(request.Model != "" && request.Model != goober.Model) || (request.Base != "" && request.Base != "parent") {
		return nil, refusal("helper_selection_unsupported", "helper", "", "helper requires the pinned Goober harness/model and parent snapshot; nondefault selection is not executable yet")
	}
	var capabilities []string
	for _, grant := range goober.Capabilities {
		if slices.Contains(v.context.ParentTask.ChildWorkflows.AllowedCapabilities, grant) && slices.Contains(v.context.GrantedCapabilities, grant) {
			capabilities = append(capabilities, grant)
		}
	}
	slices.Sort(capabilities)
	capabilities = slices.Compact(capabilities)
	sum := sha256.Sum256([]byte(key))
	document := apiv1.Workflow{
		TypeMeta:   metav1.TypeMeta{APIVersion: "goobers.dev/v1alpha1", Kind: "Workflow"},
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("helper-%x", sum[:16])},
		DSLVersion: "3.1",
		Spec: apiv1.WorkflowSpec{
			Gaggle: v.context.Gaggle.Name, Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}}, Start: "helper",
			Tasks: []apiv1.Task{{Name: "helper", Type: apiv1.TaskAgentic, Goal: request.Task, Goober: request.Goober,
				Capabilities: capabilities, PolicyActions: slices.Clone(goober.PolicyActions)}},
		},
	}
	source, err := yaml.Marshal(document)
	if err != nil {
		return nil, ErrSubmissionInvalid
	}
	// Ordinary admission remains authoritative, including credential isolation,
	// policy actions, capabilities, contracts and placement.
	if _, err := v.Validate(source); err != nil {
		return nil, err
	}
	return source, nil
}

// SubmitHelper stores generated source through the same atomic acceptance path
// as authored workflows. It creates no separate helper lifecycle or launcher.
func (s *SubmissionService) SubmitHelper(ctx context.Context, origin Origin, key string, request childworkflowwire.HelperRequest) (Submission, error) {
	authority, err := s.resolve(ctx, origin)
	if err != nil {
		return Submission{}, err
	}
	validator, err := NewValidator(authority.Admission)
	if err != nil {
		return Submission{}, err
	}
	source, err := validator.HelperSource(key, request)
	if err != nil {
		return Submission{}, err
	}
	return s.Submit(ctx, origin, SubmissionRequest{InvocationKey: key, Source: source})
}
