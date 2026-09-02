package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/journal"
)

// envelopesecrets_test.go pins the #2931 invariant the constrain-and-enforce
// contract rests on (decision record Goobers-Review/Goobernetes-v1/decisions/
// 0002): the activity arguments the engine hands Temporal — and Temporal
// persists verbatim in durable history — carry capability NAMES, never
// resolved credential values. Resolution happens worker-side, at stage start,
// through credentials.Injector; the engine dispatch boundary has no resolver
// and must never gain one, because a value that reaches history cannot be
// unwritten by any later scrubber.

const (
	pinnedCapability   = "github:pr:write"
	pinnedSecretEnvVar = "GOOBERS_TEST_ENVELOPE_PIN_TOKEN"
	pinnedSecretValue  = "ghp_pinned0123456789abcdefghijklmnopqrstuv"
)

// workerSideInjector is the real credential path a worker runs: a resolver
// over a declared token ref plus a grant binding it to the stage's declared
// capability. Building it here is what makes the assertion below meaningful —
// the value IS resolvable in this process, and still never appears in the
// envelope.
func workerSideInjector(t *testing.T, registrar credentials.SecretRegistrar) *credentials.Injector {
	t.Helper()
	resolver, err := credentials.NewResolver([]credentials.TokenRef{
		{Name: "acme/web", Env: pinnedSecretEnvVar},
	})
	if err != nil {
		t.Fatalf("build resolver: %v", err)
	}
	injector, err := credentials.NewInjector(resolver,
		[]credentials.Grant{{Capability: pinnedCapability, Ref: "acme/web"}}, registrar)
	if err != nil {
		t.Fatalf("build injector: %v", err)
	}
	return injector
}

// TestEnvelopeArgumentsCarryCapabilityNamesNotCredentialValues drives the real
// worker-side resolution and the real engine envelope build over the same
// stage, and asserts the split: the token materializes for the worker, and the
// serialized activity argument holds only the capability name that names it.
func TestEnvelopeArgumentsCarryCapabilityNamesNotCredentialValues(t *testing.T) {
	t.Setenv(pinnedSecretEnvVar, pinnedSecretValue)

	registry := journal.NewRegistryScrubber()
	injector := workerSideInjector(t, registry)
	declared := []string{pinnedCapability}

	set, err := injector.Materialize(context.Background(), declared)
	if err != nil {
		t.Fatalf("materialize worker-side credentials: %v", err)
	}
	token, err := set.Token(context.Background(), pinnedCapability)
	if err != nil {
		t.Fatalf("worker-side token for %q: %v", pinnedCapability, err)
	}
	if token != pinnedSecretValue {
		t.Fatalf("worker-side token = %q, want the resolved value; the test proves nothing if resolution did not happen", token)
	}

	in := runInput("envelope-secrets", linearSpec())
	env := buildInvocation(in, "implement", "implement the fix",
		map[string]string{"issue": "2931"}, declared, apiv1.Limits{}, nil, "coder")

	args, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal activity arguments: %v", err)
	}
	if strings.Contains(string(args), pinnedSecretValue) {
		t.Fatal("a resolved credential value appears in the serialized activity arguments; Temporal would persist it in durable history")
	}
	if !strings.Contains(string(args), pinnedCapability) {
		t.Fatalf("serialized activity arguments = %s, want the capability NAME carried as the opaque reference", args)
	}

	// The same assertion through the enforcement mechanism itself: the exact
	// -value registry every minted credential is registered with finds
	// nothing to redact, so the dispatch canary passes this envelope.
	if scrubbed := registry.Scrub(args); string(scrubbed) != string(args) {
		t.Fatal("the credential registry redacted part of the serialized envelope; the dispatch canary would refuse this dispatch")
	}
}

// TestEnvelopeArgumentsPinInputsToAuthoredValues is the other half of the
// chosen contract: because inputs are constrained rather than scrubbed, what
// the stage receives is byte-identical to what the author declared. A
// scrub-and-transform contract would have made this false — the tier-3 stage
// would see different data than the tier-1 stage, the semantic divergence
// CFG-022 forbids.
func TestEnvelopeArgumentsPinInputsToAuthoredValues(t *testing.T) {
	authored := map[string]string{"issue": "2931", "mode": "implement"}
	in := runInput("envelope-inputs", linearSpec())

	env := buildInvocation(in, "implement", "implement the fix", authored, nil, apiv1.Limits{}, nil, "coder")

	if len(env.Inputs) != len(authored) {
		t.Fatalf("envelope inputs = %v, want the authored set %v", env.Inputs, authored)
	}
	for key, want := range authored {
		if got, ok := env.Inputs[key].(string); !ok || got != want {
			t.Fatalf("envelope input %q = %v, want the authored literal %q", key, env.Inputs[key], want)
		}
	}
}
