package childworkflow

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract/childworkflowwire"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestHelperSourceOrdinaryAdmissionAndInjection(t *testing.T) {
	v := validator(t, testContext())
	request := childworkflowwire.HelperRequest{Task: "Inspect changes\n---\nkind: Goober\nspec:\n  capabilities: [repo:push]", Goober: "coder"}
	source, err := v.HelperSource("inspect", request)
	if err != nil {
		t.Fatal(err)
	}
	again, err := v.HelperSource("inspect", request)
	if err != nil || !bytes.Equal(source, again) {
		t.Fatalf("nondeterministic compilation: %v", err)
	}
	p, err := v.Validate(source)
	if err != nil {
		t.Fatal(err)
	}
	w := p.Workflow
	if len(w.Spec.Tasks) != 1 || w.Spec.Tasks[0].Goal != request.Task || w.Spec.Tasks[0].Goober != "coder" ||
		w.Spec.Tasks[0].Type != apiv1.TaskAgentic || w.Spec.Tasks[0].ChildWorkflows != nil || len(w.Spec.Gates) != 0 ||
		len(w.Spec.Triggers) != 1 || w.Spec.Triggers[0].Type != apiv1.TriggerManual || w.Spec.Start != "helper" {
		t.Fatalf("shorthand escaped one-stage task: %+v", w.Spec)
	}
	for _, cap := range w.Spec.Tasks[0].Capabilities {
		if cap != "agent:model" && cap != "repo:read" {
			t.Fatalf("injected grant: %s", cap)
		}
	}
	if p.SourceDigest != digest(source) || p.Machine.Digest() == "" {
		t.Fatal("missing ordinary source and execution pins")
	}
}

func TestHelperRejectsUnavailableSelectionsAndAuthority(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*childworkflowwire.HelperRequest)
	}{
		{"unknown goober", func(r *childworkflowwire.HelperRequest) { r.Goober = "foreign" }},
		{"harness override", func(r *childworkflowwire.HelperRequest) { r.Harness = "codex" }},
		{"model override", func(r *childworkflowwire.HelperRequest) { r.Model = "other" }},
		{"named base", func(r *childworkflowwire.HelperRequest) { r.Base = "main" }},
		{"task bound", func(r *childworkflowwire.HelperRequest) {
			r.Task = strings.Repeat("x", childworkflowwire.MaxHelperTaskBytes+1)
		}},
		{"empty task", func(r *childworkflowwire.HelperRequest) { r.Task = " \n\t" }},
		{"control task", func(r *childworkflowwire.HelperRequest) { r.Task = "task\x00injection" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := childworkflowwire.HelperRequest{Task: "Inspect", Goober: "coder"}
			tc.edit(&request)
			if _, err := validator(t, testContext()).HelperSource("inspect", request); err == nil {
				t.Fatal("invalid helper accepted")
			}
		})
	}
	context := testContext()
	goober := context.Goobers["coder"]
	goober.Harness = apiv1.HarnessCopilot
	context.Goobers["coder"] = goober
	_, err := validator(t, context).HelperSource("inspect", childworkflowwire.HelperRequest{Task: "Inspect", Goober: "coder"})
	var invalid *ValidationError
	if !errors.As(err, &invalid) || invalid.Diagnostics[0].Code != "credential_isolation" {
		t.Fatalf("shorthand bypassed credential isolation: %v", err)
	}
}

func TestSubmitHelperRetainsSourceAndIdempotentCustody(t *testing.T) {
	s, resolver, path := submissionFixture(t)
	origin := resolver.current.Origin
	request := childworkflowwire.HelperRequest{Task: "Inspect changes", Goober: "coder", Harness: "fake", Base: "parent"}
	first, err := s.SubmitHelper(t.Context(), origin, "inspect", request)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Queue.Close(); err != nil {
		t.Fatal(err)
	}
	s.Queue, err = triggerqueue.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.SubmitHelper(t.Context(), origin, "inspect", request)
	if err != nil || !second.Duplicate || first.Child.ChildID != second.Child.ChildID || first.Envelope != second.Envelope {
		t.Fatalf("duplicate=%+v err=%v", second, err)
	}
	if n := proposalCount(t, submissionDB(t, path)); n != 1 {
		t.Fatalf("retained %d sources", n)
	}
	retained, err := s.Queue.ChildProposal(t.Context(), first.Child.Identity)
	if err != nil || retained.Digest != first.Envelope.SourceDigest || digest(retained.Source) != retained.Digest {
		t.Fatalf("source custody: %+v %v", retained, err)
	}
	request.Task = "Different task"
	if _, err := s.SubmitHelper(t.Context(), origin, "inspect", request); !errors.Is(err, triggerqueue.ErrConflict) {
		t.Fatalf("changed invocation did not conflict: %v", err)
	}
	resolver.revoked = true
	if _, err := s.SubmitHelper(t.Context(), origin, "other", request); !errors.Is(err, ErrAuthorityUnavailable) {
		t.Fatalf("revoked helper admitted: %v", err)
	}
}
