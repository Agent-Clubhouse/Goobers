package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
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

	first, err := json.Marshal(SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultFailure}})
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
