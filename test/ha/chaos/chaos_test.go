package chaos

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type call struct{ stdin, line string }

type fakeExec struct {
	calls []call
	out   map[string]string
}

func (f *fakeExec) Run(_ context.Context, stdin, name string, args ...string) (string, error) {
	line := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call{stdin, line})
	for prefix, out := range f.out {
		if strings.HasPrefix(line, prefix) {
			return out, nil
		}
	}
	return "", nil
}

func TestScenarioRegistryCannotPassSilently(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, s := range Scenarios() {
		if err := s.Validate(); err != nil {
			t.Error(err)
		}
		seen[s.ID] = true
	}
	for _, id := range []string{"CHAOS-4", "CHAOS-6", "CHAOS-9", "CHAOS-10", "RESTART-RTO", "NODEKILL-RTO"} {
		if !seen[id] {
			t.Errorf("scenario %s not declared", id)
		}
	}
	for _, bad := range []Scenario{
		{ID: "X"},
		{ID: "X", Pending: "later"},
		{ID: "X", Pending: "pending #1", Run: func(context.Context, Env) error { return nil }},
	} {
		if bad.Validate() == nil {
			t.Errorf("%+v validated", bad)
		}
	}
}

func TestRunAllSkipsPendingAndGatesOnInvariants(t *testing.T) {
	t.Parallel()
	ok := func(context.Context, Env) error { return nil }
	scenarios := []Scenario{
		{ID: "B-pending", Pending: "pending #12"},
		{ID: "A-pass", Run: ok},
		{ID: "C-violation", Run: ok},
		{ID: "D-error", Run: func(context.Context, Env) error { return errors.New("boom") }},
	}
	checks := 0
	env := Env{Check: func(context.Context) (int, error) {
		checks++
		if checks == 2 {
			return 3, nil
		}
		return 0, nil
	}}
	sum, err := RunAll(context.Background(), scenarios, env)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Status{"A-pass": Passed, "B-pending": Skipped, "C-violation": Failed, "D-error": Failed}
	for _, r := range sum.Results {
		if want[r.ID] != r.Status {
			t.Errorf("%s = %s (%s), want %s", r.ID, r.Status, r.Reason, want[r.ID])
		}
	}
	if p, f, s := sum.Counts(); p != 1 || f != 2 || s != 1 {
		t.Fatalf("counts = %d/%d/%d", p, f, s)
	}
	if err := sum.WriteEvidence(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func TestRunAllRefusesUncheckedPass(t *testing.T) {
	t.Parallel()
	sum, err := RunAll(context.Background(), []Scenario{{ID: "S", Run: func(context.Context, Env) error { return nil }}}, Env{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Results[0].Status != Failed {
		t.Fatalf("status = %s, want failed without a checker", sum.Results[0].Status)
	}
}

func TestReportedMeasurementsAreNotAsserted(t *testing.T) {
	t.Parallel()
	s := Scenario{ID: "R", Run: func(_ context.Context, env Env) error { env.Report("restart-rto", 41*time.Second); return nil }}
	sum, _ := RunAll(context.Background(), []Scenario{s}, Env{Check: func(context.Context) (int, error) { return 0, nil }})
	if sum.Reported["restart-rto"] != "41s" || sum.Results[0].Status != Passed {
		t.Fatalf("%+v", sum)
	}
}

func TestSigStopUsesPIDFromCgroupNeverName(t *testing.T) {
	t.Parallel()
	ex := &fakeExec{out: map[string]string{"kubectl exec p": "42\n7\n99\n"}}
	f := &SigStop{Exec: ex, Namespace: "ns", Pod: "p", Container: "c"}
	if err := WithFault(context.Background(), f, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	var kills []string
	for _, c := range ex.calls {
		for _, banned := range []string{"pkill", "killall", "pgrep"} {
			if strings.Contains(c.line, banned) {
				t.Fatalf("name-based kill: %s", c.line)
			}
		}
		if strings.Contains(c.line, " kill ") {
			kills = append(kills, c.line[strings.Index(c.line, "kill"):])
		}
	}
	if len(kills) != 2 || kills[0] != "kill -STOP 7" || kills[1] != "kill -CONT 7" {
		t.Fatalf("kills = %v", kills)
	}
	bad := &SigStop{Exec: &fakeExec{out: map[string]string{"kubectl exec": "nginx\n"}}, Pod: "p"}
	if bad.Apply(context.Background()) == nil {
		t.Fatal("non-numeric cgroup entry accepted")
	}
}

func TestWithFaultRevertsOnFailure(t *testing.T) {
	t.Parallel()
	ex := &fakeExec{}
	f := NetworkPartition{Exec: ex, Namespace: "ns", Name_: "cut", Selector: "control"}
	want := errors.New("scenario failed")
	if err := WithFault(context.Background(), f, func(context.Context) error { return want }); !errors.Is(err, want) {
		t.Fatalf("err = %v", err)
	}
	if len(ex.calls) != 2 || !strings.Contains(ex.calls[0].stdin, "kind: NetworkPolicy") || !strings.Contains(ex.calls[1].line, "delete networkpolicy cut") {
		t.Fatalf("calls = %+v", ex.calls)
	}
}

func TestCNPGFailoverDeletesNamedPrimary(t *testing.T) {
	t.Parallel()
	ex := &fakeExec{out: map[string]string{"kubectl get cluster": "pg-2"}}
	if err := (CNPGFailover{Exec: ex, Namespace: "ns", Cluster: "pg"}).Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := ex.calls[1].line; got != "kubectl delete pod pg-2 -n ns --wait=false" {
		t.Fatalf("got %q", got)
	}
}

func TestClockSkewSetsAndClearsEnv(t *testing.T) {
	t.Parallel()
	ex := &fakeExec{}
	f := ClockSkew{Exec: ex, Namespace: "ns", Deployment: "api", Offset: "-10m"}
	if err := WithFault(context.Background(), f, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(ex.calls[0].line, "FAKETIME=-10m") || !strings.HasSuffix(ex.calls[1].line, "FAKETIME-") {
		t.Fatalf("calls = %+v", ex.calls)
	}
}

func TestToxiproxyFaults(t *testing.T) {
	t.Parallel()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
	}))
	defer srv.Close()
	tp := &Toxiproxy{BaseURL: srv.URL}
	for _, f := range []Fault{ProxyDown{T: tp, Proxy: "pg"}, Latency{T: tp, Proxy: "pg", Millis: 500}} {
		if err := WithFault(context.Background(), f, func(context.Context) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	want := "POST /proxies/pg,POST /proxies/pg,POST /proxies/pg/toxics,DELETE /proxies/pg/toxics/chaos-latency"
	if got := strings.Join(seen, ","); got != want {
		t.Fatalf("got %s", got)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer bad.Close()
	if (ProxyDown{T: &Toxiproxy{BaseURL: bad.URL}, Proxy: "pg"}).Apply(context.Background()) == nil {
		t.Fatal("500 accepted")
	}
}
