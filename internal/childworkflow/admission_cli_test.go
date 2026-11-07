package childworkflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestAdmissionCLIListsAndDecidesHeldChildren(t *testing.T) {
	s, resolver := oodSubmissionFixture(t, true)
	held, err := s.Submit(t.Context(), resolver.current.Origin, SubmissionRequest{InvocationKey: "ood", Source: []byte(validProposal)})
	if err != nil {
		t.Fatal(err)
	}
	decidedAt := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	cli := AdmissionCLI{
		Open:         func(string) (AdmissionStore, func(), error) { return s.Queue.(*triggerqueue.Store), func() {}, nil },
		DefaultActor: func() (string, error) { return "invoker", nil },
		Now:          func() time.Time { return decidedAt },
	}
	run := func(args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := cli.Run(args, flag.NewFlagSet("workflow child-admission", flag.ContinueOnError), &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}
	code, out, _ := run()
	if code != 0 || !strings.Contains(out, held.Child.ChildID) || !strings.Contains(out, "novel shape") {
		t.Fatalf("list = %d %q", code, out)
	}
	code, out, _ = run("--json")
	var reports []AdmissionReport
	if code != 0 || json.Unmarshal([]byte(out), &reports) != nil || len(reports) != 1 || reports[0].Novelty == nil || !reports[0].Novelty.OutOfDistribution || reports[0].State != "pending" {
		t.Fatalf("list json = %d %s", code, out)
	}
	if code, _, _ := run("--approve", held.Child.ChildID, "--reject", held.Child.ChildID); code != 2 {
		t.Fatalf("conflicting flags = %d", code)
	}
	code, out, _ = run("--approve", held.Child.ChildID, "--note", "reviewed", "--json")
	if code != 0 || json.Unmarshal([]byte(out), &reports) != nil || reports[0].State != "approved" || reports[0].Actor != "invoker" || reports[0].Note != "reviewed" || reports[0].DecidedAt == nil {
		t.Fatalf("approve = %d %s", code, out)
	}
	if code, _, stderr := run("--reject", held.Child.ChildID); code != 1 || !strings.Contains(stderr, "different admission decision") {
		t.Fatalf("conflicting decision = %d %s", code, stderr)
	}
	if code, out, _ := run(); code != 0 || !strings.Contains(out, "No child workflows") {
		t.Fatalf("empty list = %d %q", code, out)
	}
	if code, _, stderr := run("--approve", "child-unknown"); code != 1 || !strings.Contains(stderr, "not held") {
		t.Fatalf("unknown child = %d %s", code, stderr)
	}
}

func TestAdmissionCLIOpenFailureIsUsageError(t *testing.T) {
	cli := AdmissionCLI{Open: func(root string) (AdmissionStore, func(), error) {
		return nil, nil, errors.New("missing " + filepath.Base(root))
	}}
	var stdout, stderr bytes.Buffer
	if code := cli.Run([]string{"instance"}, flag.NewFlagSet("x", flag.ContinueOnError), &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "missing instance") {
		t.Fatalf("open failure = %d %s", code, stderr.String())
	}
}
