package runner

import (
	"encoding/json"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func restartCodecPlan(t *testing.T) StageRestartPlan {
	t.Helper()
	m := stageRestartManifest{Version: 1, EpochID: "epoch", SourceRunID: "source", SourceTerminalSeq: 9, Stage: "implement", Actor: "issuer:human", WorkflowDigest: journal.Digest([]byte("machine")), Guidance: "use saved context", GuidanceDigest: journal.Digest([]byte("use saved context"))}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return StageRestartPlan{Source: journal.RunIdentity{RunID: m.SourceRunID, WorkflowDigest: m.WorkflowDigest}, GuidanceDigest: m.GuidanceDigest, Continuation: journal.ContinuationRequest{RunID: m.EpochID, SourceRunID: m.SourceRunID, ExpectedTerminalSeq: m.SourceTerminalSeq, Operator: m.Actor, Target: m.Stage, Inputs: map[string][]byte{StageRestartInputName: raw}, InputIntegrity: map[string]apiv1.Integrity{StageRestartInputName: apiv1.IntegrityTrusted}, InputSource: map[string]string{StageRestartInputName: m.Actor}}}
}

func TestRetainedRestartPlanRefusesAuthorityAndChangedManifest(t *testing.T) {
	for name, mutate := range map[string]func(*StageRestartPlan){
		"callback": func(p *StageRestartPlan) {
			p.Continuation.VerifySourceBranch = func(string, string) error { return nil }
		},
		"child-authority": func(p *StageRestartPlan) { p.Continuation.ChildContinuation = &journal.ChildLineage{} },
		"actor":           func(p *StageRestartPlan) { p.Continuation.Operator = "other" },
		"target":          func(p *StageRestartPlan) { p.Continuation.Target = "other" },
		"source":          func(p *StageRestartPlan) { p.Source.RunID = "other" },
		"terminal":        func(p *StageRestartPlan) { p.Continuation.ExpectedTerminalSeq++ },
		"guidance":        func(p *StageRestartPlan) { p.GuidanceDigest = journal.Digest([]byte("changed")) },
		"grade": func(p *StageRestartPlan) {
			p.Continuation.InputIntegrity[StageRestartInputName] = apiv1.IntegrityUnapproved
		},
		"size": func(p *StageRestartPlan) {
			p.Continuation.Inputs["oversize"] = []byte(strings.Repeat("x", MaxStageRestartPlanBytes))
		},
	} {
		t.Run(name, func(t *testing.T) {
			plan := restartCodecPlan(t)
			mutate(&plan)
			if _, err := MarshalStageRestartPlan(plan); err == nil {
				t.Fatal("invalid plan retained")
			}
		})
	}
	plan := restartCodecPlan(t)
	raw, err := MarshalStageRestartPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "ChildWorkspace") {
		t.Fatal("runtime workspace selector entered versioned durable plan")
	}
	for _, changed := range [][]byte{append(raw, '\n'), append(raw, raw...), []byte(strings.Replace(string(raw), `"version":1`, `"version":2`, 1)), []byte(strings.Replace(string(raw), `"version":1`, `"unknown":true,"version":1`, 1))} {
		if _, err := ParseStageRestartPlan(changed); err == nil {
			t.Fatal("noncanonical/unknown plan parsed")
		}
	}
}
