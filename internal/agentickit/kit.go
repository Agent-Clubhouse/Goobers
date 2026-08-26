// Package agentickit carries everything an agentic stage needs to execute in a
// process that has no instance configuration.
//
// A mode-3 stage pod is that process. It holds no instance root by design — the
// same property that keeps a stage from reading the fleet's config — so an
// agentic stage cannot look its goober up. The kit is the alternative: the
// worker, which DOES have the config, resolves exactly one goober's execution
// inputs and hands the pod those and nothing else.
//
// TRANSPORT IS A CLAIM CHECK, not a payload. The kit is written to the blob
// plane the pod already reaches, and only its DIGEST is stamped on the pod
// spec. Two reasons, both load-bearing:
//
//   - A pod spec is readable by anything with namespace read. The kit carries
//     the run's goal, ownership boundary, context pointers and instructions;
//     none of that belongs in a listable object.
//   - Content addressing makes delivery VERIFIABLE. The pod rehashes what it
//     received and compares, so a substituted or truncated kit is detected
//     rather than executed. An endpoint keyed on run id could only return
//     "whatever the daemon says now" — there would be nothing to check it
//     against.
//
// This is the pattern Temporal itself prescribes for oversized activity
// arguments, and the storage already existed for surrender.
package agentickit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gooberassets"
)

// Grant mirrors credentials.Grant as data. Restated rather than imported so
// this package stays free of the credential machinery — it carries the
// grant's SHAPE (which goober may use which capability, and under what ref),
// never a resolved secret. No kit ever contains credential material: the pod
// resolves capabilities against the credential plane at stage start, exactly
// as a deterministic stage does.
type Grant struct {
	Goober     string `json:"goober"`
	Capability string `json:"capability"`
	Ref        string `json:"ref"`
}

// Kit is one agentic stage's complete execution input.
type Kit struct {
	// Envelope is the invocation the stage executes. It is the same value the
	// engine handed the dispatch activity, carried through rather than rebuilt.
	Envelope apiv1.InvocationEnvelope `json:"envelope"`
	// Goobers are the specs the executor routes on, keyed by goober name.
	Goobers map[string]apiv1.GooberSpec `json:"goobers"`
	// Instructions are each goober's system instructions.
	Instructions map[string]string `json:"instructions"`
	// Assets are each goober's asset bundle, materialised into the workspace
	// before invocation.
	Assets map[string]*gooberassets.WireBundle `json:"assets,omitempty"`
	// EnvCapabilities is the instance's declared environment passthrough.
	EnvCapabilities map[string]string `json:"envCapabilities,omitempty"`
	// Grants are the capability grants in force for this stage.
	Grants []Grant `json:"grants,omitempty"`
	// SandboxPosture is the instance's sandbox posture, verbatim.
	SandboxPosture string `json:"sandboxPosture,omitempty"`
}

// Marshal renders the kit and returns it with its content address.
func Marshal(k *Kit) (data []byte, digest string, err error) {
	if k == nil {
		return nil, "", fmt.Errorf("agentickit: marshal nil kit")
	}
	data, err = json.Marshal(k)
	if err != nil {
		return nil, "", fmt.Errorf("agentickit: marshal: %w", err)
	}
	return data, Digest(data), nil
}

// Digest is the content address of a marshalled kit.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Unmarshal parses a kit and VERIFIES it against the digest the pod was told
// to expect.
//
// The verification is the point of the claim check. Without it the pod would
// execute whatever the blob plane returned, and a substituted kit would run an
// agentic stage with different instructions — a silently wrong result rather
// than a failure, which is the worst outcome available here.
func Unmarshal(data []byte, wantDigest string) (*Kit, error) {
	if got := Digest(data); got != wantDigest {
		return nil, fmt.Errorf("agentickit: kit digest mismatch: got %s, expected %s", got, wantDigest)
	}
	var k Kit
	if err := json.Unmarshal(data, &k); err != nil {
		return nil, fmt.Errorf("agentickit: unmarshal: %w", err)
	}
	return &k, nil
}

// AssetBundles reconstructs the asset bundles for handing to the executor.
func (k *Kit) AssetBundles() map[string]*gooberassets.Bundle {
	if k == nil || len(k.Assets) == 0 {
		return nil
	}
	out := make(map[string]*gooberassets.Bundle, len(k.Assets))
	for name, w := range k.Assets {
		out[name] = gooberassets.FromWire(w)
	}
	return out
}
