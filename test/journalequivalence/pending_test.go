package journalequivalence

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type issueStateExecutor struct {
	state string
	err   error
	calls []string
}

func (e *issueStateExecutor) Run(_ context.Context, _ string, name string, args ...string) (string, error) {
	e.calls = append(e.calls, name+" "+strings.Join(args, " "))
	return e.state, e.err
}
func TestPendingEquivalenceReferencesExpireWhenIssueCloses(t *testing.T) {
	if len(PendingFixtures()) != 0 {
		t.Fatal("unreviewed pending fixture")
	}
	for _, test := range []struct {
		name, state string
		err         error
		wantErr     bool
	}{
		{name: "open", state: "open\n"},
		{name: "closed", state: "closed\n", wantErr: true},
		{name: "unavailable", err: errors.New("GitHub unavailable"), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			exec := &issueStateExecutor{state: test.state, err: test.err}
			err := verifyPendingFixtures(t.Context(), exec, map[string]string{"future-fixture": "1234"})
			if (err != nil) != test.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, test.wantErr)
			}
			if !reflect.DeepEqual(exec.calls, []string{"gh api repos/Agent-Clubhouse/Goobers/issues/1234 --jq .state"}) {
				t.Fatalf("unexpected issue queries: %v", exec.calls)
			}
		})
	}
}

func TestPromotedEquivalenceCorpusHasNoPendingIssueLookup(t *testing.T) {
	exec := &issueStateExecutor{}
	if err := VerifyPending(t.Context(), exec); err != nil || len(exec.calls) != 0 {
		t.Fatalf("err=%v calls=%v", err, exec.calls)
	}
}
