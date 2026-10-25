package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
	wf "github.com/goobers/goobers/internal/workflow"
)

func childContractRequest() ChildAdmissionInput {
	occurrence := journal.StageAttemptID("run-child-parent", 0, "parent", 3)
	in := ChildAdmissionInput{ParentWorkflow: "child-parent", ParentWorkflowID: "wf", ParentTemporalRunID: "trun", Stage: "parent", OriginSeq: 3}
	in.Request = journal.ChildHandoffRequest{Gaggle: "web", ParentRunID: "run-child-parent", RequestID: journal.Digest([]byte("r")), Action: "wait", ChildRunID: "child-1",
		AcceptanceID: "trigger-child-1", InvocationKey: "child-1", Origin: apiv1.ChildWorkflowOrigin{StageOccurrence: occurrence, AttemptID: occurrence}}
	in.Request.SourceDigest = journal.Digest(childSourceBytes(in.Request))
	return in
}

func encodedManifest(t *testing.T, change func(*FrozenChildManifest)) (ChildRunBinding, []byte) {
	t.Helper()
	in := childContractRequest()
	manifest := childManifestFor(in.Request)
	if change != nil {
		change(&manifest)
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return childBindingFor(in, data, time.Unix(1_700_000_000, 0).UTC()), data
}

func TestFrozenChildManifestContract(t *testing.T) {
	binding, data := encodedManifest(t, nil)
	if err := binding.validate(); err != nil {
		t.Fatalf("valid binding refused: %v", err)
	}
	child, err := decodeFrozenChildManifest(binding, data)
	if err != nil || child.RunID != "child-1" || child.WorkflowDigest == "" {
		t.Fatalf("valid manifest refused: %+v %v", child, err)
	}
	cases := map[string]func(*FrozenChildManifest){
		"annotations differ from workflow digest": func(m *FrozenChildManifest) { m.Input.Annotations = map[string]string{"example.com/policy": "changed"} },
		"source bytes differ from SourceDigest":   func(m *FrozenChildManifest) { m.Source = append(m.Source, '\n') },
		"source digest differs from accepted":     func(m *FrozenChildManifest) { m.Source = []byte("other"); m.SourceDigest = journal.Digest(m.Source) },
		"missing source bytes":                    func(m *FrozenChildManifest) { m.Source = nil },
		"missing canonical identity":              func(m *FrozenChildManifest) { m.CanonicalDigest = "" },
		"unknown manifest version":                func(m *FrozenChildManifest) { m.Version = 2 },
		"compiled digest drift":                   func(m *FrozenChildManifest) { m.Input.Spec.Tasks[0].Goal = "changed" },
		"other child run":                         func(m *FrozenChildManifest) { m.Input.RunID = "child-2" },
		"other tenant":                            func(m *FrozenChildManifest) { m.Input.Gaggle = "api" },
		"implicit preview":                        func(m *FrozenChildManifest) { m.Input.PreviewFeaturesEnabled = nil },
		"no live journal":                         func(m *FrozenChildManifest) { m.Input.LiveJournal = false },
		"unpinned kit":                            func(m *FrozenChildManifest) { m.Input.GooberDigest = "" },
		"unpinned configuration":                  func(m *FrozenChildManifest) { m.Input.ConfigGeneration = "latest" },
		"human-in-the-loop posture":               func(m *FrozenChildManifest) { m.Input.HITL = &HITLPolicy{} },
		"recursive child": func(m *FrozenChildManifest) {
			m.Input.Spec.Tasks = []apiv1.Task{{Name: "work", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "w", Next: "check", ChildWorkflows: &apiv1.ChildWorkflowPolicy{}}}
			m.Input.WorkflowDigest, _ = wf.ComputeDigest(wf.Definition{Name: m.Input.WorkflowName, Version: m.Input.Version, DSLVersion: m.Input.DSLVersion, Spec: m.Input.Spec})
		},
		"fan-out": func(m *FrozenChildManifest) {
			m.Input.Spec.Parallels = []apiv1.Parallel{{Name: "fan", Branches: []apiv1.Branch{{Name: "a", Start: "work"}}, Join: "check"}}
			m.Input.WorkflowDigest, _ = wf.ComputeDigest(wf.Definition{Name: m.Input.WorkflowName, Version: m.Input.Version, DSLVersion: m.Input.DSLVersion, Spec: m.Input.Spec})
		},
		"oversize resolved input": func(m *FrozenChildManifest) {
			m.Input.Spec.Tasks[0].Goal = strings.Repeat("x", maxChildResolvedInputBytes)
			m.Input.WorkflowDigest, _ = wf.ComputeDigest(wf.Definition{Name: m.Input.WorkflowName, Version: m.Input.Version, DSLVersion: m.Input.DSLVersion, Spec: m.Input.Spec})
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			b, data := encodedManifest(t, change)
			if _, err := decodeFrozenChildManifest(b, data); err == nil {
				t.Fatal("manifest accepted")
			}
		})
	}
	t.Run("manifest bytes differ from ManifestDigest", func(t *testing.T) {
		if _, err := decodeFrozenChildManifest(binding, append(append([]byte(nil), data...), ' ')); err == nil {
			t.Fatal("reformatted manifest replaced the accepted receipt")
		}
	})
	t.Run("proposal above the retained bound", func(t *testing.T) {
		b, data := encodedManifest(t, func(m *FrozenChildManifest) { m.Source = make([]byte, triggerqueue.MaxChildProposalBytes+1) })
		if _, err := decodeFrozenChildManifest(b, data); err == nil {
			t.Fatal("oversize proposal accepted")
		}
	})
}

func TestChildRunBindingContract(t *testing.T) {
	base, _ := encodedManifest(t, nil)
	cases := map[string]func(*ChildRunBinding){
		"non-wait action":       func(b *ChildRunBinding) { b.Request.Action = "merge" },
		"other tenant record":   func(b *ChildRunBinding) { b.Child.Identity.Gaggle = "api" },
		"other occurrence":      func(b *ChildRunBinding) { b.Child.Identity.StageOccurrence = "sta_other" },
		"other acceptance":      func(b *ChildRunBinding) { b.Child.AcceptanceID = "trigger-x" },
		"other proposal":        func(b *ChildRunBinding) { b.Child.ProposalDigest = journal.Digest([]byte("x")) },
		"mutable manifest name": func(b *ChildRunBinding) { b.ManifestDigest = "latest" },
		"no parent execution":   func(b *ChildRunBinding) { b.ParentTemporalRunID = "" },
		"unaccepted":            func(b *ChildRunBinding) { b.Child.AcceptedAt = time.Time{} },
		"deadline before start": func(b *ChildRunBinding) { b.Deadline = b.Child.AcceptedAt },
		"unbounded deadline":    func(b *ChildRunBinding) { b.Deadline = b.Child.AcceptedAt.Add(25 * time.Hour) },
		"oversize workflow name": func(b *ChildRunBinding) {
			b.ParentWorkflow = strings.Repeat("w", 257)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			b := base
			change(&b)
			if b.validate() == nil {
				t.Fatal("binding accepted")
			}
		})
	}
}

func TestLoadEngineChildFailsClosedOnCustodyLoss(t *testing.T) {
	f := newChildFixture()
	a := &Activities{EngineChildren: f}
	binding, err := a.AdmitEngineChild(t.Context(), childContractRequest())
	if err != nil {
		t.Fatal(err)
	}
	f.parentSlots = 0 // parked
	if _, err := a.LoadEngineChild(t.Context(), binding); err != nil {
		t.Fatalf("intact custody refused: %v", err)
	}
	f.corrupt = true
	if _, err := a.LoadEngineChild(t.Context(), binding); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("corrupt manifest accepted: %v", err)
	}
	f.corrupt = false
	delete(f.manifests, binding.ManifestDigest)
	if _, err := a.LoadEngineChild(t.Context(), binding); err == nil {
		t.Fatal("missing manifest accepted")
	}
}
