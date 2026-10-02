package launchreceipt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

type runtimeResolverFunc func(context.Context, Binding) (RuntimeStore, error)

func (f runtimeResolverFunc) ResolveReceiptStore(ctx context.Context, b Binding) (RuntimeStore, error) {
	return f(ctx, b)
}

func receiptFixture() Receipt {
	b := Binding{RunID: "run-1", Stage: "build", Branch: 2, StartedSeq: 7, Number: 3, Class: journal.AttemptInfra,
		WorkflowDigest: Digest([]byte("workflow")), GooberDigest: Digest([]byte("goober"))}
	b.AttemptID = journal.StageAttemptID(b.RunID, b.Branch, b.Stage, b.StartedSeq)
	return Receipt{Version: 1, Binding: b, Facts: RemoteFacts{Source: "control-plane-prepared", ImageReferenceDigest: Digest(nil), SelectorDigest: Digest(nil), ContainerCount: 1,
		Seccomp: "unknown", NetworkEnforcement: "unknown", SandboxEnforcement: "unknown", ResolvedModel: "unknown", ResolvedEffort: "unknown"}}
}

func fixtureReader(t *testing.T, kind StoreKind, receipt Receipt) (Reader, string) {
	t.Helper()
	root := t.TempDir()
	raw, err := receipt.Encode()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, receipt.Binding.AttemptID+".json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return Reader{Runtime: runtimeResolverFunc(func(context.Context, Binding) (RuntimeStore, error) { return RuntimeStore{Root: root, Kind: kind}, nil })}, path
}

func assembleFixture(t *testing.T, reader Reader, binding Binding) (Projection, attestation) {
	t.Helper()
	p, err := reader.Assemble(context.Background(), binding, MaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	var record attestation
	if err := json.Unmarshal(p.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	return p, record
}

func TestAttestationAuthorityComesOnlyFromRuntime(t *testing.T) {
	remote := receiptFixture()
	local := remote
	local.Facts = RemoteFacts{}
	facts := PreparedLocal("agent")
	facts.RequestedModelDigest = Digest([]byte("requested-only"))
	local.Local = &facts
	for _, tc := range []struct {
		name     string
		kind     StoreKind
		receipt  Receipt
		fidelity string
		reject   bool
	}{
		{"remote preparation", ControlPlaneStore, remote, "authenticated-control-plane-preparation", false},
		{"local preparation", LocalStore, local, "unverified", false},
		{"remote shaped local forgery", LocalStore, remote, "unverified", false},
		{"local in protected store", ControlPlaneStore, local, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader, _ := fixtureReader(t, tc.kind, tc.receipt)
			if tc.reject {
				if _, err := reader.Assemble(context.Background(), tc.receipt.Binding, MaxBytes); !errors.Is(err, ErrInvalid) {
					t.Fatalf("error %v", err)
				}
				return
			}
			projection, record := assembleFixture(t, reader, tc.receipt.Binding)
			if record.Schema != AttestationSchema || record.Preparation.Source != tc.kind || record.Preparation.Fidelity != tc.fidelity || record.Binding != tc.receipt.Binding {
				t.Fatalf("record %#v", record)
			}
			if record.Observed != (actuals{"unknown", "unknown", "unknown", "unknown", "unknown", "unknown", "unknown"}) {
				t.Fatalf("invented actuals: %#v", record.Observed)
			}
			if projection.Reference().Digest != journal.Digest(projection.Bytes()) || projection.Reference().Size != int64(len(projection.Bytes())) {
				t.Fatal("bad durable reference")
			}
			copy := projection.Bytes()
			copy[0] = 'x'
			if bytes.Equal(copy, projection.Bytes()) {
				t.Fatal("mutable projection")
			}
		})
	}
}

func TestAttestationMatchesEveryExpectedBindingField(t *testing.T) {
	original := receiptFixture()
	mutations := map[string]func(*Binding){
		"run": func(b *Binding) { b.RunID = "run-other" }, "stage": func(b *Binding) { b.Stage = "review" },
		"branch": func(b *Binding) { b.Branch++ }, "sequence": func(b *Binding) { b.StartedSeq++ },
		"attempt id": func(b *Binding) { b.AttemptID = "forged" }, "number": func(b *Binding) { b.Number++ },
		"class": func(b *Binding) { b.Class = journal.AttemptPolicy }, "review": func(b *Binding) { b.Review = true },
		"workflow pin": func(b *Binding) { b.WorkflowDigest = Digest([]byte("other workflow")) },
		"goober pin":   func(b *Binding) { b.GooberDigest = Digest([]byte("other goober")) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			reader, path := fixtureReader(t, ControlPlaneStore, original)
			expected := original.Binding
			mutate(&expected)
			if name != "attempt id" {
				expected.AttemptID = journal.StageAttemptID(expected.RunID, expected.Branch, expected.Stage, expected.StartedSeq)
			}
			// Copy to the expected locator, proving the full binding is checked after
			// lookup rather than relying on the name to authenticate its contents.
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(filepath.Dir(path), expected.AttemptID+".json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := reader.Assemble(context.Background(), expected, MaxBytes); !errors.Is(err, ErrInvalid) {
				t.Fatalf("cross binding accepted: %v", err)
			}
		})
	}
}

func TestAttestationReaderRejectsNoncanonicalOrUnboundedFiles(t *testing.T) {
	receipt := receiptFixture()
	raw, _ := receipt.Encode()
	for name, body := range map[string][]byte{
		"empty": {}, "truncated": raw[:len(raw)-1], "over limit": bytes.Repeat([]byte("x"), MaxBytes+1),
		"large": bytes.Repeat([]byte("x"), MaxBytes*2), "duplicate key": append([]byte(`{"version":1,`), raw[1:]...),
		"unknown key": append([]byte(`{"token":"sensitive",`), raw[1:]...), "trailing object": append(append([]byte{}, raw...), []byte(`{}`)...),
		"whitespace": append(append([]byte{}, raw...), '\n'),
	} {
		t.Run(name, func(t *testing.T) {
			reader, path := fixtureReader(t, ControlPlaneStore, receipt)
			if err := os.WriteFile(path, body, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := reader.Assemble(context.Background(), receipt.Binding, MaxBytes); !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid body accepted: %v", err)
			}
		})
	}
	t.Run("directory", func(t *testing.T) {
		reader, path := fixtureReader(t, ControlPlaneStore, receipt)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := reader.Assemble(context.Background(), receipt.Binding, MaxBytes); err == nil {
			t.Fatal("directory accepted")
		}
	})
	t.Run("escaping symlink", func(t *testing.T) {
		reader, path := fixtureReader(t, ControlPlaneStore, receipt)
		outside := filepath.Join(t.TempDir(), "outside.json")
		if err := os.WriteFile(outside, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, path); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if _, err := reader.Assemble(context.Background(), receipt.Binding, MaxBytes); err == nil || strings.Contains(err.Error(), outside) {
			t.Fatalf("escape/path disclosure: %v", err)
		}
	})
}

func TestAttestationReaderValidationCancellationAndLimits(t *testing.T) {
	receipt := receiptFixture()
	calls := 0
	reader := Reader{Runtime: runtimeResolverFunc(func(context.Context, Binding) (RuntimeStore, error) {
		calls++
		return RuntimeStore{}, errors.New("secret /private/path")
	})}
	invalid := receipt.Binding
	invalid.AttemptID = "../escape"
	if _, err := reader.Assemble(context.Background(), invalid, MaxBytes); !errors.Is(err, ErrInvalid) || calls != 0 {
		t.Fatalf("path before validation: %v %d", err, calls)
	}
	for _, limit := range []int{-1, 0, MaxBytes + 1} {
		if _, err := reader.Assemble(context.Background(), receipt.Binding, limit); !errors.Is(err, ErrInvalid) || calls != 0 {
			t.Fatalf("limit %d: %v calls %d", limit, err, calls)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := reader.Assemble(ctx, receipt.Binding, MaxBytes); !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("cancel: %v %d", err, calls)
	}
	if _, err := reader.Assemble(context.Background(), receipt.Binding, MaxBytes); err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "/") {
		t.Fatalf("resolver error disclosure: %v", err)
	}
	reader, _ = fixtureReader(t, ControlPlaneStore, receipt)
	raw, _ := receipt.Encode()
	for _, limit := range []int{len(raw) - 1, len(raw)} {
		if _, err := reader.Assemble(context.Background(), receipt.Binding, limit); !errors.Is(err, ErrInvalid) {
			t.Fatalf("input/output bound %d: %v", limit, err)
		}
	}
	reader.Runtime = runtimeResolverFunc(func(context.Context, Binding) (RuntimeStore, error) {
		cancel()
		return RuntimeStore{Root: t.TempDir(), Kind: ControlPlaneStore}, nil
	})
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	if _, err := reader.Assemble(ctx, receipt.Binding, MaxBytes); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled resolver: %v", err)
	}
}

func TestAttestationMissingRemoteNeverFallsBackToMatchingLocal(t *testing.T) {
	receipt := receiptFixture()
	_, localPath := fixtureReader(t, LocalStore, receipt)
	remoteRoot := filepath.Join(filepath.Dir(localPath), "runtime-launch-receipts") // name grants no authority
	calls := 0
	reader := Reader{Runtime: runtimeResolverFunc(func(context.Context, Binding) (RuntimeStore, error) {
		calls++
		return RuntimeStore{Root: remoteRoot, Kind: ControlPlaneStore}, nil
	})}
	_, record := assembleFixture(t, reader, receipt.Binding)
	if _, err := os.Stat(remoteRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read created a store: %v", err)
	}
	if calls != 1 || record.Preparation.Fidelity != "unknown" || record.Preparation.Availability != "missing" || record.Preparation.Remote != nil {
		t.Fatalf("fallback/invented evidence: %#v, calls %d", record, calls)
	}
	reader.Runtime = runtimeResolverFunc(func(context.Context, Binding) (RuntimeStore, error) { return RuntimeStore{Kind: UnknownStore}, nil })
	receipt.Binding.WorkflowDigest = ""
	receipt.Binding.GooberDigest = ""
	_, record = assembleFixture(t, reader, receipt.Binding)
	if record.Preparation.Source != UnknownStore || record.Preparation.Availability != "missing" {
		t.Fatalf("legacy evidence: %#v", record)
	}
}

func TestAttestationReferenceRestartAndForgery(t *testing.T) {
	receipt := receiptFixture()
	reader, path := fixtureReader(t, ControlPlaneStore, receipt)
	first, _ := assembleFixture(t, reader, receipt.Binding)
	var persisted Reference
	raw, _ := json.Marshal(first.Reference())
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	restarted := Reader{Runtime: runtimeResolverFunc(func(context.Context, Binding) (RuntimeStore, error) {
		return RuntimeStore{Root: filepath.Dir(path), Kind: ControlPlaneStore}, nil
	})}
	second, err := restarted.Revalidate(context.Background(), receipt.Binding, persisted, MaxBytes)
	if err != nil || second.Reference() != first.Reference() || !bytes.Equal(second.Bytes(), first.Bytes()) {
		t.Fatalf("restart changed bytes/ref: %v", err)
	}
	for name, mutate := range map[string]func(*Reference){"name": func(r *Reference) { r.Name = "other" }, "digest": func(r *Reference) { r.Digest = journal.Digest([]byte(`{"fidelity":"authenticated"}`)) }, "size": func(r *Reference) { r.Size++ }} {
		t.Run(name, func(t *testing.T) {
			forged := persisted
			mutate(&forged)
			if _, err := restarted.Revalidate(context.Background(), receipt.Binding, forged, MaxBytes); !errors.Is(err, ErrInvalid) {
				t.Fatalf("forgery: %v", err)
			}
		})
	}
	// A generic journal artifact and forged body cannot be promoted after the
	// trusted source disappears, even when its name and binding appear correct.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Revalidate(context.Background(), receipt.Binding, persisted, MaxBytes); !errors.Is(err, ErrInvalid) {
		t.Fatalf("artifact authority substituted: %v", err)
	}
}

func TestAttestationAttemptSeparation(t *testing.T) {
	seen := map[string]bool{}
	for _, tc := range []struct {
		stage  string
		branch int
		seq    uint64
		number int
		review bool
	}{
		{"build", 0, 1, 1, false}, {"review", 0, 2, 1, true}, {"build", 0, 3, 2, false}, {"build", 1, 4, 1, false},
	} {
		receipt := receiptFixture()
		b := &receipt.Binding
		b.Stage, b.Branch, b.StartedSeq, b.Number, b.Review = tc.stage, tc.branch, tc.seq, tc.number, tc.review
		b.AttemptID = journal.StageAttemptID(b.RunID, b.Branch, b.Stage, b.StartedSeq)
		reader, _ := fixtureReader(t, ControlPlaneStore, receipt)
		projection, _ := assembleFixture(t, reader, *b)
		if seen[projection.Reference().Name] {
			t.Fatal("attempts collided")
		}
		seen[projection.Reference().Name] = true
	}
}

func TestAttestationNoRawSecretsPathsOrInventedActuals(t *testing.T) {
	receipt := receiptFixture()
	receipt.Facts = RemoteFacts{}
	local := PreparedLocal("agent")
	local.HarnessDigest = Digest([]byte("/private/bin/secret-harness"))
	local.RequestedModelDigest = Digest([]byte("token=secret-model"))
	receipt.Local = &local
	reader, _ := fixtureReader(t, LocalStore, receipt)
	projection, _ := assembleFixture(t, reader, receipt.Binding)
	for _, secret := range []string{"/private", "secret-harness", "secret-model", "token=", "prompt", "command", "authenticated"} {
		if bytes.Contains(projection.Bytes(), []byte(secret)) {
			t.Fatalf("leaked %q", secret)
		}
	}
	for _, secret := range []string{"/private/path", "token=secret", "prompt text", "command --secret"} {
		receipt.Local.HarnessDigest = secret
		if _, err := receipt.Encode(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("raw secret allowed: %v", err)
		}
	}
}
