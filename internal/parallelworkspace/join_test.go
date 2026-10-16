package parallelworkspace

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/parallelworkspace/spec"
	"github.com/goobers/goobers/internal/worktree"
)

const (
	testRun    = "run-join-1"
	testGaggle = "gaggle-a"
	testPar    = "fan"
)

func newRun(t *testing.T) *journal.Run {
	t.Helper()
	run, err := journal.Create(t.TempDir(), journal.RunIdentity{RunID: testRun, Gaggle: testGaggle}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	return run
}

func reader(t *testing.T, run *journal.Run) *journal.Reader {
	t.Helper()
	r, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func record(t *testing.T, run *journal.Run, name string, data []byte, integrity apiv1.Integrity) journal.Ref {
	t.Helper()
	ref, err := run.RecordArtifactBoundedWithIntegrity(name, data, integrity, spec.MaxJoinMetadataBytes)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func joinAnnotation(t *testing.T, run *journal.Run, phase string, receipt spec.JoinReceipt) error {
	t.Helper()
	return run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Parallel: testPar, Runner: map[string]any{"kind": spec.JoinKind, "phase": phase, "join": receipt}})
}

func baseRequest() spec.JoinRequest {
	return spec.JoinRequest{Request: spec.Request{RunID: testRun, Gaggle: testGaggle, Parallel: testPar, Sequence: 1}}
}

func TestValidateJoinIdentity(t *testing.T) {
	rd := reader(t, newRun(t))
	tests := []struct {
		name    string
		mutate  func(*spec.JoinRequest)
		wantErr bool
	}{
		{"match", func(*spec.JoinRequest) {}, false},
		{"run changed", func(r *spec.JoinRequest) { r.RunID = "other" }, true},
		{"gaggle changed", func(r *spec.JoinRequest) { r.Gaggle = "other" }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := baseRequest()
			tt.mutate(&request)
			err := validateJoinIdentity(rd, request)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "root journal identity") {
				t.Fatalf("unexpected error %v", err)
			}
		})
	}
}

func TestValidateJoinRequest(t *testing.T) {
	run := newRun(t)
	trusted := record(t, run, "plan.json", []byte(`{"Version":1}`), apiv1.IntegrityTrusted)
	corrupt := record(t, run, "corrupt.json", []byte(`{not json`), apiv1.IntegrityTrusted)
	untrusted := record(t, run, "untrusted.json", []byte(`{"Version":1}`), apiv1.IntegrityUnapproved)
	missing := journal.Ref{Path: "gone.json", Digest: journal.Digest([]byte("gone")), Size: 4, Integrity: apiv1.IntegrityTrusted}
	rd := reader(t, run)
	tests := []struct {
		name    string
		mutate  func(*spec.JoinRequest)
		wantErr string
	}{
		{"identity mismatch", func(r *spec.JoinRequest) { r.Gaggle = "other" }, "root journal identity"},
		{"zero plan ref", func(*spec.JoinRequest) {}, "host provenance"},
		{"untrusted plan", func(r *spec.JoinRequest) { r.Plan = untrusted }, "host provenance"},
		{"missing plan artifact", func(r *spec.JoinRequest) { r.Plan = missing }, ""},
		{"corrupt plan artifact", func(r *spec.JoinRequest) { r.Plan = corrupt }, ""},
		{"seed without host provenance", func(r *spec.JoinRequest) { r.Plan = trusted }, "host provenance"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := baseRequest()
			tt.mutate(&request)
			err := validateJoinRequest(rd, request)
			if err == nil {
				t.Fatal("expected error")
			}
			if tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestJoinReceiptsRejectBadArtifacts(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, *journal.Run) spec.JoinReceipt
	}{
		{"missing intent artifact", func(t *testing.T, run *journal.Run) spec.JoinReceipt {
			return spec.JoinReceipt{Sequence: 1, Intent: journal.Ref{Path: "x", Digest: journal.Digest([]byte("x")), Size: 1, Integrity: apiv1.IntegrityTrusted}}
		}},
		{"untrusted intent artifact", func(t *testing.T, run *journal.Run) spec.JoinReceipt {
			return spec.JoinReceipt{Sequence: 1, Intent: record(t, run, "i.json", []byte(`{}`), apiv1.IntegrityUnapproved)}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := newRun(t)
			if err := run.Append(journal.Event{Type: journal.EventParallelStarted, Parallel: testPar}); err != nil {
				t.Fatal(err)
			}
			receipt := tt.setup(t, run)
			if err := joinAnnotation(t, run, "prepared", receipt); err != nil {
				t.Fatal(err)
			}
			rd := reader(t, run)
			if _, err := spec.ReadJoins(rd); err == nil {
				t.Fatal("ReadJoins accepted bad intent")
			}
			if err := (Service{}).RecoverJoins(context.Background(), run); err == nil {
				t.Fatal("RecoverJoins accepted bad intent")
			}
		})
	}
}

func preparedRun(t *testing.T, intent []byte) (*journal.Run, spec.JoinReceipt) {
	t.Helper()
	run := newRun(t)
	if err := run.Append(journal.Event{Type: journal.EventParallelStarted, Parallel: testPar}); err != nil {
		t.Fatal(err)
	}
	receipt := spec.JoinReceipt{Sequence: 1, Intent: record(t, run, "parallel-join-intent.json", intent, apiv1.IntegrityTrusted)}
	if err := joinAnnotation(t, run, "prepared", receipt); err != nil {
		t.Fatal(err)
	}
	return run, receipt
}

func TestRecoverJoinsResumesRecordedIntent(t *testing.T) {
	request := baseRequest()
	request.Plan = journal.Ref{Path: "plan", Integrity: apiv1.IntegrityUnapproved}
	data, err := json.Marshal(joinIntent{Version: 1, Request: request})
	if err != nil {
		t.Fatal(err)
	}
	run, _ := preparedRun(t, data)
	pending, err := spec.PendingJoins(reader(t, run))
	if err != nil || len(pending) != 1 || pending[0].Sequence != 1 || pending[0].Applied {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	// Recovery must re-run validation of the recorded intent before applying it.
	err = (Service{}).RecoverJoins(context.Background(), run)
	if err == nil || !strings.Contains(err.Error(), "host provenance") {
		t.Fatalf("err = %v", err)
	}
}

func TestRecoverJoinsSkipsAppliedAndEmpty(t *testing.T) {
	run := newRun(t)
	if err := (Service{}).RecoverJoins(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	run, receipt := preparedRun(t, []byte(`{}`))
	ready := record(t, run, "parallel-join-application.json", []byte(`{}`), apiv1.IntegrityTrusted)
	receipt.Ready = ready
	if err := joinAnnotation(t, run, "ready", receipt); err != nil {
		t.Fatal(err)
	}
	if err := joinAnnotation(t, run, "applied", receipt); err != nil {
		t.Fatal(err)
	}
	pending, err := spec.PendingJoins(reader(t, run))
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	if err := (Service{}).RecoverJoins(context.Background(), run); err != nil {
		t.Fatal(err)
	}
}

func TestReadJoinsRejectsBadTransitions(t *testing.T) {
	tests := []struct {
		name   string
		phases []string
	}{
		{"duplicate prepared", []string{"prepared"}},
		{"duplicate ready", []string{"ready", "ready"}},
		{"unknown phase", []string{"bogus"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run, receipt := preparedRun(t, []byte(`{}`))
			receipt.Ready = record(t, run, "ready.json", []byte(`{}`), apiv1.IntegrityTrusted)
			for _, phase := range tt.phases {
				r := receipt
				if phase == "prepared" {
					r.Ready = journal.Ref{}
				}
				if err := joinAnnotation(t, run, phase, r); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := spec.ReadJoins(reader(t, run)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestReadJoinIntentScope(t *testing.T) {
	good := baseRequest()
	tests := []struct {
		name    string
		intent  joinIntent
		wantErr string
	}{
		{"version", joinIntent{Version: 2, Request: good}, "changed scope"},
		{"sequence", joinIntent{Version: 1, Request: func() spec.JoinRequest { r := good; r.Sequence = 9; return r }()}, "changed scope"},
		{"parallel", joinIntent{Version: 1, Request: func() spec.JoinRequest { r := good; r.Parallel = "other"; return r }()}, "changed scope"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.intent)
			if err != nil {
				t.Fatal(err)
			}
			run, _ := preparedRun(t, data)
			rd := reader(t, run)
			states, err := spec.ReadJoins(rd)
			if err != nil {
				t.Fatal(err)
			}
			_, err = readJoinIntent(rd, *states[1])
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	run, _ := preparedRun(t, []byte(`{corrupt`))
	rd := reader(t, run)
	states, err := spec.ReadJoins(rd)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readJoinIntent(rd, *states[1]); err == nil {
		t.Fatal("corrupt intent accepted")
	}
}

func TestJoinFailsClosedWithoutJournal(t *testing.T) {
	run := newRun(t)
	dir := run.Dir()
	if err := (Service{}).Join(context.Background(), dirRecorder{Run: run, dir: dir + "-missing"}, baseRequest()); err == nil {
		t.Fatal("expected error")
	}
	if err := (Service{}).RecoverJoins(context.Background(), dirRecorder{Run: run, dir: dir + "-missing"}); err == nil {
		t.Fatal("expected error")
	}
	if err := (Service{}).Join(context.Background(), run, baseRequest()); err == nil {
		t.Fatal("expected validation error")
	}
}

type dirRecorder struct {
	*journal.Run
	dir string
}

func (d dirRecorder) Dir() string { return d.dir }

func TestJoinRootRequiresServices(t *testing.T) {
	run := newRun(t)
	if _, _, err := (Service{}).joinRoot(context.Background(), run, baseRequest()); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("err = %v", err)
	}
	s := Service{Worktrees: &worktree.Manager{}, CloneURL: func(apiv1.RepoRef) (string, error) { return "https://example.test/r.git", nil }}
	if _, _, err := s.joinRoot(context.Background(), run, baseRequest()); err == nil || !strings.Contains(err.Error(), "physical owner changed") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidateJoinEvidence(t *testing.T) {
	plan := journal.Ref{Path: "plan.json", Digest: journal.Digest([]byte("p")), Size: 1, Integrity: apiv1.IntegrityTrusted}
	src := spec.Source{SnapshotSHA: "abc"}
	request := baseRequest()
	request.Plan = plan
	request.Sequence = 2
	request.Results = []spec.JoinResult{{Branch: 1, Status: journal.BranchSucceeded, Source: src}}
	planned := journal.Event{Type: journal.EventRunnerAnnotation, Parallel: testPar, Runner: map[string]any{"kind": "isolated.parent.fork.planned", "plan": plan}}
	result := func(status journal.BranchStatus, source spec.Source) journal.Event {
		return journal.Event{Type: journal.EventRunnerAnnotation, Parallel: testPar, Branch: 1, Runner: map[string]any{"kind": "isolated.parent.fork.result", "result": map[string]any{"Sequence": 2, "Plan": plan, "Branch": 1, "Status": status, "Source": source}}}
	}
	finished := func(status journal.BranchStatus) journal.Event {
		return journal.Event{Type: journal.EventBranchFinished, Parallel: testPar, Branch: 1, BranchStatus: status}
	}
	tests := []struct {
		name    string
		events  []journal.Event
		wantErr bool
	}{
		{"complete", []journal.Event{planned, result(journal.BranchSucceeded, src), finished(journal.BranchSucceeded)}, false},
		{"no plan", []journal.Event{result(journal.BranchSucceeded, src), finished(journal.BranchSucceeded)}, true},
		{"no finish", []journal.Event{planned, result(journal.BranchSucceeded, src)}, true},
		{"finish without result", []journal.Event{planned, finished(journal.BranchSucceeded)}, true},
		{"substituted source", []journal.Event{planned, result(journal.BranchSucceeded, spec.Source{SnapshotSHA: "other"}), finished(journal.BranchSucceeded)}, true},
		{"status mismatch", []journal.Event{planned, result(journal.BranchFailed, src), finished(journal.BranchFailed)}, true},
		{"duplicate plan", []journal.Event{planned, planned, result(journal.BranchSucceeded, src), finished(journal.BranchSucceeded)}, true},
		{"duplicate result", []journal.Event{planned, result(journal.BranchSucceeded, src), result(journal.BranchSucceeded, src), finished(journal.BranchSucceeded)}, true},
		{"finished status differs", []journal.Event{planned, result(journal.BranchSucceeded, src), finished(journal.BranchFailed)}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := newRun(t)
			if err := run.Append(journal.Event{Type: journal.EventParallelStarted, Parallel: testPar}); err != nil {
				t.Fatal(err)
			}
			for _, event := range tt.events {
				if err := run.Append(event); err != nil {
					t.Fatal(err)
				}
			}
			err := validateJoinEvidence(reader(t, run), request)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
