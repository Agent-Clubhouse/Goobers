package chaos

import (
	"context"
	"strings"
	"testing"
	"time"
)

// smokeExec scripts a cluster: heartbeat lines come from beats, one per logs call.
type smokeExec struct {
	fakeExec
	beats   []string
	helpers []string // successive answers to the final helper query; the last repeats
}

func (s *smokeExec) Run(ctx context.Context, stdin, name string, args ...string) (string, error) {
	line := name + " " + strings.Join(args, " ")
	switch {
	case strings.HasPrefix(line, "kubectl logs"):
		s.calls = append(s.calls, call{stdin, line})
		b := s.beats[0]
		s.beats = s.beats[1:]
		return b + "\n", nil
	case strings.HasPrefix(line, "kubectl get pods -n goobers-chaos") && strings.HasSuffix(line, "-o name"):
		s.calls = append(s.calls, call{stdin, line})
		out := ""
		if len(s.helpers) > 0 {
			out = s.helpers[0]
			if len(s.helpers) > 1 {
				s.helpers = s.helpers[1:]
			}
		}
		return out, nil
	}
	return s.fakeExec.Run(ctx, stdin, name, args...)
}

func newSmokeExec(beats ...string) *smokeExec {
	cg := "/host/sys/fs/cgroup/kubepods/cri-containerd-" + testContainerID + ".scope"
	return &smokeExec{beats: beats, fakeExec: fakeExec{out: map[string]string{
		"kubectl get pod freeze-target -n goobers-chaos-target -o jsonpath={.status.containerStatuses": "containerd://" + testContainerID,
		"kubectl get pod freeze-target -n goobers-chaos-target -o jsonpath={.spec.nodeName}":           "kind-worker",
		"kubectl exec chaos-freezer-0123456789ab -n goobers-chaos -- sh -c find":                       cg,
		"kubectl exec chaos-freezer-0123456789ab -n goobers-chaos -- sh -c echo":                       "frozen 1\n",
	}}}
}

func noSleep(time.Duration) {}

func TestFreezeSmokeHappyPath(t *testing.T) {
	t.Parallel()
	// before freeze, frozen (x2, same), after thaw (advanced)
	ex := newSmokeExec("100", "103", "103", "108")
	// Revert verifies frozen 0: flip the scripted events once Apply has frozen.
	ev, err := FreezeSmoke(context.Background(), &thawOnRevert{smokeExec: ex}, noSleep)
	if err != nil {
		t.Fatalf("%v\nevidence: %v", err, ev)
	}
	joined := strings.Join(ev, "\n")
	for _, want := range []string{"frozen 1", "heartbeat stalled while frozen at 103", "frozen 0", "heartbeat resumed at 108", "sweep at end: no helper pods remain"} {
		if !strings.Contains(joined, want) {
			t.Errorf("evidence lacks %q:\n%s", want, joined)
		}
	}
}

// thawOnRevert answers cgroup.events with frozen 0 once the freeze script that
// writes 0 has run.
type thawOnRevert struct{ *smokeExec }

func (t *thawOnRevert) Run(ctx context.Context, stdin, name string, args ...string) (string, error) {
	line := name + " " + strings.Join(args, " ")
	if strings.Contains(line, "echo 0 >") {
		t.out["kubectl exec chaos-freezer-0123456789ab -n goobers-chaos -- sh -c echo"] = "frozen 0\n"
	}
	return t.smokeExec.Run(ctx, stdin, name, args...)
}

func TestFreezeSmokeFailsWhenHeartbeatKeepsRunning(t *testing.T) {
	t.Parallel()
	ex := newSmokeExec("100", "103", "107", "108")
	_, err := FreezeSmoke(context.Background(), &thawOnRevert{smokeExec: ex}, noSleep)
	if err == nil || !strings.Contains(err.Error(), "advanced while frozen") {
		t.Fatalf("err = %v", err)
	}
}

func TestFreezeSmokeFailsWhenHeartbeatDoesNotResume(t *testing.T) {
	t.Parallel()
	ex := newSmokeExec("100", "103", "103", "103")
	_, err := FreezeSmoke(context.Background(), &thawOnRevert{smokeExec: ex}, noSleep)
	if err == nil || !strings.Contains(err.Error(), "did not resume") {
		t.Fatalf("err = %v", err)
	}
}

func TestFreezeSmokeFailsWhenHelperRemains(t *testing.T) {
	t.Parallel()
	ex := newSmokeExec("100", "103", "103", "108")
	ex.helpers = []string{"pod/chaos-freezer-x\n"}
	_, err := FreezeSmoke(context.Background(), &thawOnRevert{smokeExec: ex}, noSleep)
	if err == nil || !strings.Contains(err.Error(), "helper pods remain") {
		t.Fatalf("err = %v", err)
	}
}

func TestFreezeSmokeWaitsForTerminatingHelper(t *testing.T) {
	t.Parallel()
	ex := newSmokeExec("100", "103", "103", "108")
	ex.helpers = []string{"pod/chaos-freezer-x\n", "pod/chaos-freezer-x\n", ""}
	if _, err := FreezeSmoke(context.Background(), &thawOnRevert{smokeExec: ex}, noSleep); err != nil {
		t.Fatal(err)
	}
}
