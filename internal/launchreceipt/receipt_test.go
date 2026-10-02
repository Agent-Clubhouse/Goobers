package launchreceipt

import (
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestReceiptRejectsUnboundedOrUnobservedClaims(t *testing.T) {
	base := Receipt{Version: 1, Binding: Binding{RunID: "run-1", Stage: "build", StartedSeq: 1, Number: 1, AttemptID: journal.StageAttemptID("run-1", 0, "build", 1)}, Facts: RemoteFacts{Source: "control-plane-prepared", ImageReferenceDigest: Digest(nil), SelectorDigest: Digest(nil), ContainerCount: 1, Seccomp: "unknown", NetworkEnforcement: "unknown", SandboxEnforcement: "unknown", ResolvedModel: "unknown", ResolvedEffort: "unknown"}}
	if _, err := base.Encode(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Receipt){
		"forged identity":     func(r *Receipt) { r.Binding.AttemptID = "../../escape" },
		"path in pin":         func(r *Receipt) { r.Binding.WorkflowDigest = "/sensitive/host/path" },
		"actual model":        func(r *Receipt) { r.Facts.ResolvedModel = "requested-model" },
		"actual network":      func(r *Receipt) { r.Facts.NetworkEnforcement = "enforced" },
		"actual sandbox":      func(r *Receipt) { r.Facts.SandboxEnforcement = "enforced" },
		"too many mounts":     func(r *Receipt) { r.Facts.WritableMountCount = 129 },
		"too many containers": func(r *Receipt) { r.Facts.ContainerCount = 33 },
		"raw image":           func(r *Receipt) { r.Facts.ImageReferenceDigest = "registry/token@image" },
		"model source":        func(r *Receipt) { r.Facts.Source = "model-output" },
	} {
		t.Run(name, func(t *testing.T) {
			r := base
			mutate(&r)
			if _, err := r.Encode(); err == nil {
				t.Fatal("unsafe receipt accepted")
			}
		})
	}
}
