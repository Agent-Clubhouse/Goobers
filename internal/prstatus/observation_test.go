package prstatus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

func TestVerdictObservationClassification(t *testing.T) {
	reader := providers.NewGitHubProvider("fixture")
	for _, tc := range []struct{ name, state, summary, wantState, wantBase, wantReason string }{
		{"pass", "passing", "pass for base b: ok", "pass", "matched", "complete-read"},
		{"fail", "failing", "fail for base b: fix", "fail", "matched", "complete-read"},
		{"pending", "pending", "defer for base b: wait", "pending", "matched", "complete-read"},
		{"withdrawn", "pending", "stale for base b: wait", "pending", "matched", "complete-read"},
		{"old-base", "passing", "pass for base old: ok", "pass", "stale", "complete-read"},
		{"malformed", "passing", "secret-sentinel", "pass", "unparseable", "malformed-verdict"},
		{"contradiction", "passing", "fail for base b", "pass", "unparseable", "malformed-verdict"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			poll := providers.PullRequestPollResult{VerdictReadStatus: providers.VerdictReadComplete, VerdictStatus: &providers.CheckDetail{State: providers.CheckState(tc.state), Summary: tc.summary}}
			o := ObserveVerdict(reader, poll, nil, "b")
			if o.Presence != "present" || o.State != tc.wantState || o.BaseMatch != tc.wantBase || o.Reason != tc.wantReason || o.WouldBlockMissingVerdict(true) {
				t.Fatalf("observation=%+v", o)
			}
			if strings.Contains(fmt.Sprint(o), "sentinel") {
				t.Fatal("description leaked")
			}
		})
	}
	poll := providers.PullRequestPollResult{VerdictReadStatus: providers.VerdictReadComplete}
	absent := ObserveVerdict(reader, poll, nil, "b")
	if absent.Presence != "absent" || !absent.WouldBlockMissingVerdict(true) || absent.WouldBlockMissingVerdict(false) {
		t.Fatal(absent)
	}
	for _, o := range []VerdictObservation{ObserveVerdict(struct{}{}, poll, nil, "b"), ObserveVerdict(reader, providers.PullRequestPollResult{}, nil, "b")} {
		if o.Presence == "absent" || o.WouldBlockMissingVerdict(true) {
			t.Fatalf("unsupported or unknown counted absent: %+v", o)
		}
	}
}

func TestVerdictObservationErrorClassesAreBounded(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{context.Canceled, "cancelled"}, {context.DeadlineExceeded, "deadline"},
		{&json.SyntaxError{}, "decode"}, {&json.UnmarshalTypeError{}, "decode"}, {io.ErrUnexpectedEOF, "decode"},
		{&net.DNSError{Err: "secret-sentinel", IsTimeout: true}, "transient"},
		{errors.New("secret-sentinel"), "poll-error"},
	} {
		o := ObserveVerdict(struct{}{}, providers.PullRequestPollResult{PollErrorPhase: "secret-sentinel"}, tc.err, "b")
		if o.Reason != tc.want || o.Phase != "unknown" || o.Presence != "unreadable" || o.WouldBlockMissingVerdict(true) || strings.Contains(fmt.Sprint(o), "sentinel") {
			t.Fatalf("observation=%+v want=%s", o, tc.want)
		}
	}
}
