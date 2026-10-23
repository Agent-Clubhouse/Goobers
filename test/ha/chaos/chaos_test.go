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
	for _, id := range []string{"CHAOS-4", "CHAOS-6", "CHAOS-9", "CHAOS-10", "CHAOS-13", "RESTART-RTO", "NODEKILL-RTO", "UPGRADE-11"} {
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

const testContainerID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func freezeExec(events string) *fakeExec {
	return &fakeExec{out: map[string]string{
		"kubectl get pod p -n ns -o jsonpath={.status.containerStatuses":         "containerd://" + testContainerID,
		"kubectl get pod p -n ns -o jsonpath={.spec.nodeName}":                   "node-1",
		"kubectl exec chaos-freezer-0123456789ab -n goobers-chaos -- sh -c find": "/host/sys/fs/cgroup/kubepods/pod/cri-containerd-" + testContainerID + ".scope",
		"kubectl exec chaos-freezer-0123456789ab -n goobers-chaos -- sh -c echo": events,
	}}
}

func TestCgroupFreezeFreezesFromNodeAndVerifies(t *testing.T) {
	t.Parallel()
	ex := freezeExec("frozen 1\n")
	f := &CgroupFreeze{Exec: ex, Namespace: "ns", Pod: "p", Container: "c", HelperImage: "img@sha256:x"}
	if err := f.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	var manifest, nsManifest string
	for _, c := range ex.calls {
		if strings.HasPrefix(c.line, "kubectl apply") && strings.Contains(c.stdin, "kind: Namespace") {
			nsManifest = c.stdin
		}
		if strings.Contains(c.line, "-n ns ") && !strings.HasPrefix(c.line, "kubectl get pod p") {
			t.Fatalf("helper work touched the target namespace: %s", c.line)
		}
		for _, banned := range []string{"pkill", "killall", "pgrep", "kill -", "-- kill"} {
			if strings.Contains(c.line, banned) {
				t.Fatalf("signal path used: %s", c.line)
			}
		}
		if strings.HasPrefix(c.line, "kubectl apply") && strings.Contains(c.stdin, "kind: Pod") {
			manifest = c.stdin
		}
	}
	if !strings.Contains(nsManifest, "name: goobers-chaos") || !strings.Contains(nsManifest, "pod-security.kubernetes.io/enforce: privileged") {
		t.Errorf("chaos namespace manifest = %q", nsManifest)
	}
	for _, want := range []string{"namespace: goobers-chaos", "managed-by: goobers-chaos", "nodeName: node-1", "hostPID: true", "privileged: true", "image: img@sha256:x"} {
		if !strings.Contains(manifest, want) {
			t.Errorf("helper manifest lacks %q:\n%s", want, manifest)
		}
	}
	// Revert thaws, verifies and deletes the helper.
	ex.out["kubectl exec chaos-freezer-0123456789ab -n goobers-chaos -- sh -c echo"] = "frozen 0\n"
	if err := f.Revert(context.Background()); err != nil {
		t.Fatal(err)
	}
	if last := ex.calls[len(ex.calls)-1].line; !strings.HasPrefix(last, "kubectl delete pod chaos-freezer-0123456789ab") {
		t.Fatalf("helper not deleted, last call %q", last)
	}
}

func TestCgroupFreezeFailsWhenEffectNotObserved(t *testing.T) {
	t.Parallel()
	for _, events := range []string{"frozen 0\n", ""} {
		ex := freezeExec(events)
		f := &CgroupFreeze{Exec: ex, Namespace: "ns", Pod: "p", Container: "c", HelperImage: "img"}
		if f.Apply(context.Background()) == nil {
			t.Fatalf("events %q accepted as frozen", events)
		}
	}
	bad := freezeExec("frozen 1")
	bad.out["kubectl get pod p -n ns -o jsonpath={.status.containerStatuses"] = "containerd://nginx"
	if (&CgroupFreeze{Exec: bad, Namespace: "ns", Pod: "p", Container: "c", HelperImage: "img"}).Apply(context.Background()) == nil {
		t.Fatal("non-ID container accepted")
	}
	if (&CgroupFreeze{Exec: freezeExec(""), Namespace: "ns", Pod: "p", Container: "c"}).Apply(context.Background()) == nil {
		t.Fatal("missing helper image accepted")
	}
	if (&CgroupFreeze{Exec: freezeExec(""), Namespace: "ns", Pod: "p", Container: `c")]; x`, HelperImage: "img"}).Apply(context.Background()) == nil {
		t.Fatal("jsonpath-injecting container name accepted")
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

func TestClockSkewWritesOffsetFileWithoutRestart(t *testing.T) {
	t.Parallel()
	ex := &fakeExec{out: map[string]string{"kubectl exec api-0": "-10m"}}
	f := ClockSkew{Exec: ex, Namespace: "ns", Pod: "api-0", Container: "c", Offset: "-10m"}
	if err := f.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range ex.calls {
		if strings.Contains(c.line, "set env") || strings.Contains(c.line, "rollout") {
			t.Fatalf("skew restarts pods: %s", c.line)
		}
	}
	if !strings.Contains(ex.calls[0].line, "faketime") || !strings.Contains(ex.calls[0].line, "'-10m'") {
		t.Fatalf("call = %s", ex.calls[0].line)
	}
	ex.out["kubectl exec api-0"] = "+0s"
	if err := f.Revert(context.Background()); err != nil {
		t.Fatal(err)
	}
	ex.out["kubectl exec api-0"] = "garbage"
	if f.Apply(context.Background()) == nil {
		t.Fatal("unverified read-back accepted")
	}
	f.Offset = "-10m; rm -rf /"
	if f.Apply(context.Background()) == nil {
		t.Fatal("injected offset accepted")
	}
}

func TestVerifyPendingOpenFailsOnMergedReference(t *testing.T) {
	t.Parallel()
	ex := &fakeExec{out: map[string]string{
		"gh api repos/Agent-Clubhouse/Goobers/issues/1": "open",
		"gh api repos/Agent-Clubhouse/Goobers/issues/2": "closed",
	}}
	if err := VerifyPendingOpen(context.Background(), ex, []string{"1"}); err != nil {
		t.Fatal(err)
	}
	err := VerifyPendingOpen(context.Background(), ex, []string{"1", "2"})
	if err == nil || !strings.Contains(err.Error(), "#2 is closed") {
		t.Fatalf("err = %v", err)
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

func TestSweepThawsAndDeletesLeftoverHelpers(t *testing.T) {
	t.Parallel()
	cg := "/host/sys/fs/cgroup/kubepods/cri-containerd-" + testContainerID + ".scope"
	ex := &fakeExec{out: map[string]string{
		"kubectl get pods -n goobers-chaos":                   "chaos-freezer-a=" + cg + "\nchaos-freezer-b=\n",
		"kubectl exec chaos-freezer-a -n goobers-chaos -- sh": "frozen 0\n",
	}}
	if err := Sweep(context.Background(), ex, DefaultChaosNamespace); err != nil {
		t.Fatal(err)
	}
	var thawed, deleted []string
	for _, c := range ex.calls {
		switch {
		case strings.Contains(c.line, "cgroup.freeze"):
			thawed = append(thawed, c.line)
		case strings.HasPrefix(c.line, "kubectl delete pod"):
			deleted = append(deleted, strings.Fields(c.line)[3])
		}
	}
	if len(thawed) != 1 || !strings.Contains(thawed[0], "echo 0 > "+cg) {
		t.Fatalf("thawed = %v", thawed)
	}
	if strings.Join(deleted, ",") != "chaos-freezer-a,chaos-freezer-b" {
		t.Fatalf("deleted = %v", deleted)
	}
}

func TestSweepKeepsHelperWhenThawIsNotVerified(t *testing.T) {
	t.Parallel()
	cg := "/host/sys/fs/cgroup/kubepods/cri-containerd-" + testContainerID + ".scope"
	ex := &fakeExec{out: map[string]string{
		"kubectl get pods -n goobers-chaos":                   "chaos-freezer-a=" + cg + "\n",
		"kubectl exec chaos-freezer-a -n goobers-chaos -- sh": "frozen 1\n",
	}}
	if Sweep(context.Background(), ex, DefaultChaosNamespace) == nil {
		t.Fatal("unverified thaw reported clean")
	}
	for _, c := range ex.calls {
		if strings.HasPrefix(c.line, "kubectl delete pod") {
			t.Fatalf("helper deleted while its cgroup may still be frozen: %s", c.line)
		}
	}
	unsafe := &fakeExec{out: map[string]string{"kubectl get pods": "p=/host/sys/fs/cgroup/x; rm -rf /\n"}}
	if Sweep(context.Background(), unsafe, DefaultChaosNamespace) == nil {
		t.Fatal("unsafe annotation accepted")
	}
}

func TestSweepWithNothingLeftIsClean(t *testing.T) {
	t.Parallel()
	ex := &fakeExec{}
	if err := Sweep(context.Background(), ex, DefaultChaosNamespace); err != nil || len(ex.calls) != 1 {
		t.Fatalf("err=%v calls=%d", err, len(ex.calls))
	}
}
