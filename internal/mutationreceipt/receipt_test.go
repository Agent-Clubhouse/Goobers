package mutationreceipt

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestIdentityBindsContentAndProviderTarget(t *testing.T) {
	base, err := New("github", "https://api.example.test/repos/acme/app", "comment", "issue/7", map[string]string{"body": "private original body", "decision": "approve"})
	if err != nil {
		t.Fatal(err)
	}
	same, err := New("github", base.Repository, base.Action, base.Target, map[string]string{"decision": "approve", "body": "private original body"})
	if err != nil || same != base {
		t.Fatalf("unstable map encoding: %#v %v", same, err)
	}
	structIdentity, err := New("github", base.Repository, base.Action, base.Target, struct {
		Decision string `json:"decision"`
		Body     string `json:"body"`
	}{Decision: "approve", Body: "private original body"})
	if err != nil || structIdentity != base {
		t.Fatalf("struct ordering changed semantic digest: %v %#v", err, structIdentity)
	}
	large, _ := New("github", base.Repository, base.Action, base.Target, map[string]uint64{"number": 9007199254740992})
	otherLarge, _ := New("github", base.Repository, base.Action, base.Target, map[string]uint64{"number": 9007199254740993})
	if large == otherLarge {
		t.Fatal("large integers lost precision")
	}
	raw, _ := json.Marshal(base)
	if strings.Contains(string(raw), "private original body") {
		t.Fatal("plaintext leaked into receipt")
	}
	for _, field := range []string{"body", "provider", "repository", "action", "target"} {
		t.Run(field, func(t *testing.T) {
			candidate := base
			switch field {
			case "body":
				candidate, err = New(base.Provider, base.Repository, base.Action, base.Target, map[string]string{"body": "changed", "decision": "approve"})
			case "provider":
				candidate.Provider = "gitea"
			case "repository":
				candidate.Repository = "https://other.test/repos/acme/app"
			case "action":
				candidate.Action = "review"
			case "target":
				candidate.Target = "pr/7"
			}
			if err != nil || candidate == base {
				t.Fatalf("identity failed to bind %s: %v", field, err)
			}
		})
	}
	for _, repository := range []string{"acme/app", "https://user:secret@forge.test/repos/a/b", "https://forge.test/repos/a/b?token=secret", "https://forge.test/repos/a/b#secret"} {
		if _, err := New("github", repository, "comment", "issue/7", "body"); err == nil {
			t.Fatalf("accepted unsafe repository %q", repository)
		}
	}
}

type captureRecorder struct {
	receipts     []Receipt
	failPhase    string
	beforeRecord func(Receipt)
}

func (r *captureRecorder) RecordSemanticMutation(_ context.Context, receipt Receipt) error {
	if r.beforeRecord != nil {
		r.beforeRecord(receipt)
	}
	if receipt.Phase == r.failPhase {
		return errors.New("durability failure")
	}
	r.receipts = append(r.receipts, receipt)
	return nil
}

func TestCaptureCrashBoundaries(t *testing.T) {
	mutation, err := New("github", "https://api.example.test/repos/a/b", "comment", "issue/7", "body")
	if err != nil {
		t.Fatal(err)
	}
	for _, boundary := range []string{"intent failure", "action failure", "completion failure", "success", "cancel after success"} {
		t.Run(boundary, func(t *testing.T) {
			recorder := &captureRecorder{}
			switch boundary {
			case "intent failure":
				recorder.failPhase = "intent"
			case "completion failure":
				recorder.failPhase = "completed"
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			err := Capture(ctx, recorder, "source", mutation, func(context.Context, Receipt) error {
				calls++
				if len(recorder.receipts) != 1 || recorder.receipts[0].Phase != "intent" {
					t.Fatal("action ran before durable intent")
				}
				if boundary == "action failure" {
					return errors.New("response lost after remote commit")
				}
				if boundary == "cancel after success" {
					cancel()
				}
				return nil
			})
			wantCalls, wantRecords := 1, 1
			if boundary == "intent failure" {
				wantCalls, wantRecords = 0, 0
			}
			success := boundary == "success" || boundary == "cancel after success"
			if success {
				wantRecords = 2
			}
			if (err == nil) != success || calls != wantCalls || len(recorder.receipts) != wantRecords {
				t.Fatalf("err=%v calls=%d records=%#v", err, calls, recorder.receipts)
			}
			if success && (recorder.receipts[1].Phase != "completed" || recorder.receipts[0].ID != recorder.receipts[1].ID) {
				t.Fatal("completion does not bind intent")
			}
		})
	}
}

func TestCapturePanicAndIdenticalInvocations(t *testing.T) {
	mutation, _ := New("github", "https://api.example.test/repos/a/b", "comment", "issue/7", "body")
	recorder := &captureRecorder{}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected injected crash")
			}
		}()
		_ = Capture(context.Background(), recorder, "source", mutation, func(context.Context, Receipt) error { panic("crash after commit") })
	}()
	if len(recorder.receipts) != 1 || recorder.receipts[0].Phase != "intent" {
		t.Fatal("crash invented completion")
	}
	for range 2 {
		if err := Capture(context.Background(), recorder, "continuation", mutation, func(context.Context, Receipt) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if len(recorder.receipts) != 5 || recorder.receipts[1].ID == recorder.receipts[3].ID {
		t.Fatal("capture suppressed a fresh invocation or reused its identity")
	}
}
