package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func testPlane(t *testing.T) *SurrenderDir {
	t.Helper()
	plane, err := NewSurrenderDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return plane
}

// The surrender plane round-trips one attempt's result document by identity,
// and distinct attempts never collide: a retried attempt (fresh Number, fresh
// pod — D1) reads its own surrender, never a stale one.
func TestSurrenderDirRoundTripByAttemptIdentity(t *testing.T) {
	ctx := context.Background()
	plane := testPlane(t)

	first, err := json.Marshal(SurrenderedResult{Result: apiv1.ResultEnvelope{
		Status: apiv1.ResultFailure,
		Error:  &apiv1.ErrorInfo{Code: "stage_failed", Message: "first attempt failed"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(SurrenderedResult{
		Result:    apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "second attempt"},
		Mutations: []SurrenderedMutation{{Provider: "github", Kind: "pr", ID: "9"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := plane.Put(ctx, "run-1", "build", 1, first); err != nil {
		t.Fatal(err)
	}
	if err := plane.Put(ctx, "run-1", "build", 2, second); err != nil {
		t.Fatal(err)
	}

	got, err := ReadSurrenderedResult(ctx, plane, "run-1", "build", 2)
	if err != nil {
		t.Fatalf("read attempt 2: %v", err)
	}
	if got.Result.Status != apiv1.ResultSuccess || got.Result.Summary != "second attempt" {
		t.Fatalf("attempt 2 result = %+v, want its own surrender, never attempt 1's", got.Result)
	}
	if len(got.Mutations) != 1 || got.Mutations[0].ID != "9" {
		t.Fatalf("mutations = %+v, want carried through", got.Mutations)
	}
	if prior, err := ReadSurrenderedResult(ctx, plane, "run-1", "build", 1); err != nil || prior.Result.Status != apiv1.ResultFailure {
		t.Fatalf("attempt 1 = %+v (%v), want the first attempt's own document intact", prior, err)
	}
}

// An absent surrender is the distinguished ErrNoSurrender — the signal the
// engine classifies as an infra-attempt data-plane fault, distinct from an
// unreadable plane.
func TestSurrenderDirAbsentIsErrNoSurrender(t *testing.T) {
	plane := testPlane(t)
	_, err := ReadSurrenderedResult(context.Background(), plane, "run-x", "build", 1)
	if !errors.Is(err, ErrNoSurrender) {
		t.Fatalf("error = %v, want ErrNoSurrender", err)
	}
	if _, err := plane.Get(context.Background(), "", "build", 1); err == nil {
		t.Fatal("an empty run id must be refused, not resolved to a path")
	}
}

// The disposal gate answers presence from the plane: unconfirmed before the
// pod surrenders, confirmed after — the exact question Dispatch asks before
// disposing the pod.
func TestPlaneSurrenderGateConfirmed(t *testing.T) {
	ctx := context.Background()
	plane := testPlane(t)
	attempt := Attempt{RunID: "run-g", Stage: "build", Number: 1}
	gate := PlaneSurrenderGate{Plane: plane}

	confirmed, err := gate.Confirmed(ctx, attempt)
	if err != nil || confirmed {
		t.Fatalf("Confirmed = %v %v before surrender, want false", confirmed, err)
	}
	if err := plane.Put(ctx, "run-g", "build", 1, []byte(`{"result":{"status":"success"}}`)); err != nil {
		t.Fatal(err)
	}
	confirmed, err = gate.Confirmed(ctx, attempt)
	if err != nil || !confirmed {
		t.Fatalf("Confirmed = %v %v after surrender, want true", confirmed, err)
	}
	if _, err := (PlaneSurrenderGate{}).Confirmed(ctx, attempt); err == nil {
		t.Fatal("a gate with no plane must fail closed")
	}
}

// Put is idempotent per attempt identity: a pod retrying its own surrender
// (the only legal duplicate — one attempt is one pod) succeeds without
// rewriting.
func TestSurrenderDirPutIdempotent(t *testing.T) {
	ctx := context.Background()
	plane := testPlane(t)
	doc := []byte(`{"result":{"status":"success"}}`)
	if err := plane.Put(ctx, "run-i", "build", 1, doc); err != nil {
		t.Fatal(err)
	}
	if err := plane.Put(ctx, "run-i", "build", 1, []byte(`{"result":{"status":"failure"}}`)); err != nil {
		t.Fatalf("duplicate Put must succeed as a no-op: %v", err)
	}
	got, err := plane.Get(ctx, "run-i", "build", 1)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(doc) {
		t.Fatalf("document = %s, want the first write kept (write-once per key)", got)
	}
}

// A surrendered document that decodes but does not match the contract's shape
// is refused at the read (#3838): the pod is attacker-reachable compute whose
// only contract with the engine is this document, so a fabricated mutation, an
// unknown status, or an escaping artifact pointer must never become the
// engine's record of the attempt.
func TestReadSurrenderedResultRefusesMalformedDocuments(t *testing.T) {
	ctx := context.Background()
	digest := apiv1.Digest([]byte("bytes"))
	tests := []struct {
		name string
		doc  SurrenderedResult
	}{
		{name: "unknown status", doc: SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultStatus("done")}}},
		{name: "empty status", doc: SurrenderedResult{}},
		{name: "failure without error", doc: SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultFailure}}},
		{
			name: "escaping artifact pointer",
			doc: SurrenderedResult{Result: apiv1.ResultEnvelope{
				Status:    apiv1.ResultSuccess,
				Artifacts: []apiv1.ArtifactPointer{{Path: "../../etc/passwd", Digest: digest}},
			}},
		},
		{
			name: "mutation naming no provider",
			doc: SurrenderedResult{
				Result:    apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
				Mutations: []SurrenderedMutation{{Kind: "pr", ID: "9"}},
			},
		},
		{
			name: "mutation naming no id",
			doc: SurrenderedResult{
				Result:    apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
				Mutations: []SurrenderedMutation{{Provider: "github", Kind: "pr"}},
			},
		},
		{
			name: "empty mutation issue",
			doc: SurrenderedResult{
				Result:         apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
				MutationIssues: []string{""},
			},
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plane := testPlane(t)
			data, err := json.Marshal(tt.doc)
			if err != nil {
				t.Fatal(err)
			}
			if err := plane.Put(ctx, "run-m", "build", i+1, data); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadSurrenderedResult(ctx, plane, "run-m", "build", i+1); err == nil {
				t.Fatalf("ReadSurrenderedResult accepted %s; want the read to fail closed", tt.name)
			}
		})
	}
}

func TestReadSurrenderedResultRejectsInvalidWorkspaceRevision(t *testing.T) {
	valid := `{"repository":{"provider":"github","owner":"org","name":"repo"},"commitSha":"` + strings.Repeat("a", 40) + `"}`
	for _, status := range []apiv1.ResultStatus{apiv1.ResultSuccess, apiv1.ResultFailure, apiv1.ResultBlocked, apiv1.ResultNoWork} {
		for name, control := range map[string]string{
			"scalar":             `"untrusted"`,
			"bad-identity":       strings.Replace(valid, `"github"`, `"ado"`, 1),
			"bad-sha":            strings.Replace(valid, strings.Repeat("a", 40), "short", 1),
			"unknown-control":    strings.TrimSuffix(valid, "}") + `,"unknown":true}`,
			"unknown-repository": strings.Replace(valid, `"name":"repo"`, `"name":"repo","credential":"unexpected"`, 1),
			"unknown-base":       strings.TrimSuffix(valid, "}") + `,"baseRepository":{"provider":"github","owner":"org","name":"repo","unexpected":true}}`,
		} {
			t.Run(string(status)+"/"+name, func(t *testing.T) {
				plane := testPlane(t)
				data := []byte(`{"result":{"status":"` + string(status) + `","error":{"code":"test","message":"test"},"workspaceRevision":` + control + `}}`)
				if err := plane.Put(context.Background(), "revision", "stage", 1, data); err != nil {
					t.Fatal(err)
				}
				if _, err := ReadSurrenderedResult(context.Background(), plane, "revision", "stage", 1); err == nil {
					t.Fatal("invalid revision passed surrendered-result admission")
				}
			})
		}
	}
}

func TestReadSurrenderedResultPreservesRevisionAndEnvelopeExtensions(t *testing.T) {
	valid := `{"repository":{"provider":"github","owner":"org","name":"repo"},"commitSha":"` + strings.Repeat("a", 40) + `"}`
	for _, status := range []apiv1.ResultStatus{apiv1.ResultSuccess, apiv1.ResultFailure, apiv1.ResultBlocked, apiv1.ResultNoWork} {
		for _, selected := range []bool{false, true} {
			control := ""
			if selected {
				control = `,"workspaceRevision":` + valid
			}
			plane := testPlane(t)
			data := []byte(`{"extension":true,"result":{"status":"` + string(status) + `","extension":true,"outputs":{"legacy":"kept"},"error":{"code":"test","message":"test"}` + control + `}}`)
			if err := plane.Put(context.Background(), "revision", "stage", 1, data); err != nil {
				t.Fatal(err)
			}
			got, err := ReadSurrenderedResult(context.Background(), plane, "revision", "stage", 1)
			if err != nil {
				t.Fatalf("status %s selected %v: %v", status, selected, err)
			}
			if got.Result.Status != status || got.Result.Outputs["legacy"] != "kept" || (got.Result.WorkspaceRevision != nil) != selected {
				t.Fatalf("result changed during admission: %+v", got.Result)
			}
			if selected && got.Result.WorkspaceRevision.CommitSHA != strings.Repeat("a", 40) {
				t.Fatalf("revision changed during admission: %+v", got.Result.WorkspaceRevision)
			}
		}
	}
}

// The shapes a well-behaved pod surrenders keep round-tripping: validation
// tightens what a pod may claim without narrowing the legitimate contract.
func TestReadSurrenderedResultAcceptsContractShapes(t *testing.T) {
	ctx := context.Background()
	digest := apiv1.Digest([]byte("bytes"))
	docs := []SurrenderedResult{
		{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}},
		{Result: apiv1.ResultEnvelope{Status: apiv1.ResultNoWork, Summary: "nothing to do"}},
		{Result: apiv1.ResultEnvelope{Status: apiv1.ResultBlocked}},
		{
			Result: apiv1.ResultEnvelope{
				Status:     apiv1.ResultSuccess,
				Artifacts:  []apiv1.ArtifactPointer{{Path: "artifacts/build/out.txt", Digest: digest}},
				Transcript: &apiv1.ArtifactPointer{Path: "artifacts/build/transcript.md", Digest: digest},
			},
			Mutations:      []SurrenderedMutation{{Provider: "github", Kind: "pull-request", ID: "9", Operation: "create"}},
			MutationIssues: []string{"malformed provenance line 3"},
			WorkspaceDelta: digest,
		},
		{
			Result:  apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
			Verdict: &apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "not yet"},
		},
	}
	for i, doc := range docs {
		data, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		plane := testPlane(t)
		if err := plane.Put(ctx, "run-ok", "build", i+1, data); err != nil {
			t.Fatal(err)
		}
		got, err := ReadSurrenderedResult(ctx, plane, "run-ok", "build", i+1)
		if err != nil {
			t.Fatalf("document %d: %v", i, err)
		}
		if got.Result.Status != doc.Result.Status {
			t.Fatalf("document %d status = %q, want %q", i, got.Result.Status, doc.Result.Status)
		}
	}
}
