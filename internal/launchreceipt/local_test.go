package launchreceipt

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func localFixture() (Receipt, apiv1.InvocationEnvelope) {
	facts := PreparedLocal("agent")
	binding := Binding{RunID: "local-run", Stage: "build", StartedSeq: 7, Number: 1, WorkflowDigest: Digest([]byte("pinned workflow")), GooberDigest: Digest([]byte("pinned goober"))}
	binding.AttemptID = journal.StageAttemptID(binding.RunID, binding.Branch, binding.Stage, binding.StartedSeq)
	return Receipt{Version: 1, Binding: binding, Local: &facts}, apiv1.InvocationEnvelope{RunID: binding.RunID, TaskID: binding.Stage, Attempt: 1}
}

func TestLocalRecorderPersistsImmutableUnverifiedReceipt(t *testing.T) {
	receipt, _ := localFixture()
	root := filepath.Join(t.TempDir(), "private-receipts")
	if err := (LocalRecorder{Root: root}).Record(context.Background(), receipt); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, receipt.Binding.AttemptID+".json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(before), `"integrity":"unverified"`) || !strings.Contains(string(before), `"resolvedModel":"unknown"`) {
		t.Fatal("receipt claimed unsupported fidelity")
	}
	// A new runtime authority after restart cannot overwrite the consumed ID.
	receipt.Local.HarnessDigest = Digest([]byte("other runtime"))
	if err := (LocalRecorder{Root: root}).Record(context.Background(), receipt); !errors.Is(err, ErrUsed) {
		t.Fatalf("restart replay: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("consumed receipt changed")
	}
}

func TestLocalAuthorityRejectsReplayExpiryCrossAttemptAndFacts(t *testing.T) {
	receipt, _ := localFixture()
	raw, err := receipt.Encode()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	fresh := func() *localAuthority {
		return &localAuthority{token: "fixture-single-use-local-token", grant: Grant{AttemptID: receipt.Binding.AttemptID, Digest: Digest(raw), Expires: now.Add(time.Minute).Unix()}, now: func() time.Time { return now }}
	}
	authority := fresh()
	if _, err := authority.VerifyLaunchGrant("goobers-launch.forged-controller-token"); err == nil {
		t.Fatal("foreign bearer accepted")
	}
	if _, err := authority.VerifyLaunchGrant("fixture-single-use-local-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := authority.VerifyLaunchGrant("fixture-single-use-local-token"); err == nil {
		t.Fatal("stolen grant replayed after consumption")
	}
	expired := fresh()
	expired.grant.Expires = now.Unix()
	if _, err := expired.VerifyLaunchGrant("fixture-single-use-local-token"); err == nil {
		t.Fatal("expired grant accepted")
	}
	for _, mutation := range []string{"attempt", "role", "facts"} {
		t.Run(mutation, func(t *testing.T) {
			other, _ := localFixture()
			switch mutation {
			case "attempt":
				other.Binding.StartedSeq++
				other.Binding.AttemptID = journal.StageAttemptID(other.Binding.RunID, 0, other.Binding.Stage, other.Binding.StartedSeq)
			case "role":
				other.Binding.Review = true
			case "facts":
				other.Local.HarnessDigest = Digest([]byte("forged"))
			}
			store, err := NewStore(t.TempDir(), fresh())
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Accept(context.Background(), "fixture-single-use-local-token", other); !errors.Is(err, ErrInvalid) {
				t.Fatalf("changed %s accepted: %v", mutation, err)
			}
		})
	}
}

func TestLocalReceiptRejectsSecretPathsAndFabricatedActuals(t *testing.T) {
	for _, field := range []string{"source", "integrity", "kind", "harness", "version", "model", "effort", "sandbox", "sandbox-enforced", "network", "grants", "resolved-model", "resolved-effort"} {
		t.Run(field, func(t *testing.T) {
			receipt, _ := localFixture()
			const unsafe = "/private/fixture-secret-token"
			f := receipt.Local
			switch field {
			case "source":
				f.Source = unsafe
			case "integrity":
				f.Integrity = "attested"
			case "kind":
				f.Kind = unsafe
			case "harness":
				f.HarnessDigest = unsafe
			case "version":
				f.HarnessVersionDigest = unsafe
			case "model":
				f.RequestedModelDigest = unsafe
			case "effort":
				f.RequestedEffortDigest = unsafe
			case "sandbox":
				f.PreparedSandbox = unsafe
			case "sandbox-enforced":
				f.SandboxEnforcement = "enforced"
			case "network":
				f.NetworkEnforcement = "enforced"
			case "grants":
				f.DirectoryGrants = unsafe
			case "resolved-model":
				f.ResolvedModel = "requested-is-not-resolved"
			case "resolved-effort":
				f.ResolvedEffort = "high"
			}
			if _, err := receipt.Encode(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("unsafe %s accepted", field)
			}
		})
	}
}

type localDeterministicFunc func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error)

func (f localDeterministicFunc) Run(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	return f(ctx, env, run)
}

func TestLocalDeterministicGateCoversShellAndEveryBuiltin(t *testing.T) {
	receipt, env := localFixture()
	ctx := WithBinding(context.Background(), receipt.Binding)
	for _, kind := range []string{"", "shell", "ci-poll", "external-telemetry", "future-safe-looking-kind"} {
		t.Run(kind, func(t *testing.T) {
			env.Inputs = map[string]any{"kind": kind}
			started := false
			next := localDeterministicFunc(func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
				started = true
				return apiv1.ResultEnvelope{}, nil
			})
			guard := GuardDeterministic(next, LocalRecorder{Root: t.TempDir()}, true)
			_, err := guard.Run(ctx, env, apiv1.DeterministicRun{Command: []string{"goobers", "trusted-looking-builtin"}})
			if !errors.Is(err, ErrControllerKey) || started {
				t.Fatalf("key host kind=%q: started=%t err=%v", kind, started, err)
			}
		})
	}
}

func TestLocalDeterministicGateRequiresDurableReceiptBeforeExecution(t *testing.T) {
	receipt, env := localFixture()
	ctx := WithBinding(context.Background(), receipt.Binding)
	root := t.TempDir()
	started := 0
	next := localDeterministicFunc(func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		started++
		if _, err := os.Stat(filepath.Join(root, receipt.Binding.AttemptID+".json")); err != nil {
			t.Fatal("execution preceded persistence")
		}
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	})
	guard := GuardDeterministic(next, LocalRecorder{Root: root}, false)
	if _, err := guard.Run(context.Background(), env, apiv1.DeterministicRun{}); !errors.Is(err, ErrLocalPersistence) || started != 0 {
		t.Fatal("missing authority launched")
	}
	if _, err := guard.Run(ctx, env, apiv1.DeterministicRun{}); err != nil || started != 1 {
		t.Fatalf("keyless: %v / %d", err, started)
	}
	if _, err := guard.Run(ctx, env, apiv1.DeterministicRun{}); !errors.Is(err, ErrLocalPersistence) || started != 1 {
		t.Fatal("duplicate attempt launched")
	}
	blockedRoot := filepath.Join(t.TempDir(), "private-secret-path")
	if err := os.WriteFile(blockedRoot, nil, 0600); err != nil {
		t.Fatal(err)
	}
	guard = GuardDeterministic(next, LocalRecorder{Root: blockedRoot}, false)
	if _, err := guard.Run(ctx, env, apiv1.DeterministicRun{}); !errors.Is(err, ErrLocalPersistence) || strings.Contains(err.Error(), blockedRoot) || started != 1 {
		t.Fatal("persistence failure launched or leaked path")
	}
}
