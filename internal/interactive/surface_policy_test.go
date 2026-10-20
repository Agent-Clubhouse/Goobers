package interactive

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func attemptCommand(s *Service, owner, sessionID string, cmd Command) error {
	current, err := s.Session(owner, sessionID)
	if err != nil {
		return err
	}
	cmd.CommandID = id("cmd_")
	cmd.ExpectedSequence, cmd.ExpectedFence = current.Sequence, current.Fence
	_, err = s.Command(context.Background(), owner, sessionID, cmd)
	return err
}

func TestSurfacePolicyBindingAndChildAttenuation(t *testing.T) {
	e := newExecutor()
	opts := options(t, e)
	policy := &opts.Surfaces[0]
	policy.ConfigRevision, policy.Gaggle, policy.Goober = "revision-one", "team", "helper"
	policy.Model, policy.Harness = "chosen-model", "chosen-harness"
	policy.Tools, policy.Workflows, policy.AuthorityRefs = []string{"repo.read", "ado.read"}, []string{"test"}, []string{"ado-personal"}
	policy.ChildCommandCapabilities = []string{"go.test"}
	policy.LeaseSeconds, policy.MaxTurnSeconds = 3, 5
	s := openTest(t, opts)
	session := createTest(t, s, "alice")
	if session.ConfigRevision != policy.ConfigRevision {
		t.Fatal("session lost configuration revision")
	}
	commandTest(t, s, "alice", session.ID, Command{Type: "message", Text: "Investigate"})
	run := receiveRun(t, e)
	if run.req.ConfigRevision != policy.ConfigRevision || run.req.Model != policy.Model || run.req.Harness != policy.Harness || run.req.Goober != policy.Goober || run.req.Gaggle != policy.Gaggle || !reflect.DeepEqual(run.req.Tools, policy.Tools) || !reflect.DeepEqual(run.req.AuthorityRefs, policy.AuthorityRefs) || !reflect.DeepEqual(run.req.Workflows, policy.Workflows) {
		t.Fatalf("unbound execution: %+v", run.req)
	}
	bound, err := s.SurfaceForSession("alice", session.ID)
	if err != nil {
		t.Fatal(err)
	}
	bound.Tools[0] = "mutated"
	fresh, _ := s.SurfaceForSession("alice", session.ID)
	if fresh.Tools[0] != "repo.read" {
		t.Fatal("surface lookup leaked mutable configuration")
	}
	_, err = s.SurfaceForSession("bob", session.ID)
	requireCode(t, err, "not_found")
	run.result <- ExecutionResult{Message: "done"}
	waitStatus(t, s, "alice", session.ID, "idle")
	proposal := commandTest(t, s, "alice", session.ID, Command{Type: "child.request", Text: "Review the result"})
	commandTest(t, s, "alice", session.ID, Command{Type: "child.review", ChildID: proposal.ChildID, Decision: "approve"})
	child := receiveRun(t, e)
	if !child.req.ReadOnly || child.req.Model != policy.Model || child.req.Harness != policy.Harness || child.req.ConfigRevision != policy.ConfigRevision || len(child.req.AuthorityRefs)+len(child.req.Tools)+len(child.req.Workflows) != 0 || !reflect.DeepEqual(child.req.ChildCommandCapabilities, policy.ChildCommandCapabilities) {
		t.Fatalf("child policy was not attenuated: %+v", child.req)
	}
	_, err = s.RequestChildFromExecution(context.Background(), "alice", session.ID, child.req.TurnID, child.req.LeaseID, child.req.Fence, "nested", "more", "")
	requireCode(t, err, "child_depth_denied")
	child.result <- ExecutionResult{Message: "reviewed"}
}

func TestSurfaceExecutionModeIntersectionNoFallback(t *testing.T) {
	opts := options(t, newExecutor())
	opts.AvailableExecutionModes = []string{ExecutionModeLocalTools, ExecutionModeCodexLocal}
	opts.Surfaces[0].ExecutionModes = []string{ExecutionModeCodexLocal}
	opts.Surfaces[0].DefaultExecutionMode = ExecutionModeCodexLocal
	s := openTest(t, opts)
	_, err := s.Create("alice", CreateRequest{CommandID: "demo", SurfaceID: "work", ExecutionMode: ExecutionModeLocalTools})
	requireCode(t, err, "execution_mode_unavailable")
	created := createTest(t, s, "alice")
	if created.ExecutionMode != ExecutionModeCodexLocal {
		t.Fatal("surface default did not pin model mode")
	}
	opts.StateDir = t.TempDir()
	opts.AvailableExecutionModes = []string{ExecutionModeLocalTools}
	unavailable := openTest(t, opts)
	_, err = unavailable.Create("alice", CreateRequest{CommandID: "no-fallback", SurfaceID: "work"})
	requireCode(t, err, "execution_mode_unavailable")
}

func TestSurfaceCapacitySeparatesOwnersAndSurfaces(t *testing.T) {
	e := newExecutor()
	opts := options(t, e)
	opts.MaxActive, opts.MaxActivePerOwner = 8, 5
	opts.Surfaces[0].MaxActive, opts.Surfaces[0].MaxActivePerOwner = 2, 1
	s := openTest(t, opts)
	a := createTest(t, s, "alice")
	a2 := createTest(t, s, "alice")
	b := createTest(t, s, "bob")
	c := createTest(t, s, "charlie")
	commandTest(t, s, "alice", a.ID, Command{Type: "message", Text: "work"})
	first := receiveRun(t, e)
	requireCode(t, attemptCommand(s, "alice", a2.ID, Command{Type: "message", Text: "more"}), "capacity")
	helper, err := s.Create("alice", CreateRequest{CommandID: "helper", SurfaceID: "help"})
	if err != nil {
		t.Fatal(err)
	}
	commandTest(t, s, "alice", helper.ID, Command{Type: "message", Text: "other surface"})
	second := receiveRun(t, e)
	commandTest(t, s, "bob", b.ID, Command{Type: "message", Text: "work"})
	third := receiveRun(t, e)
	requireCode(t, attemptCommand(s, "charlie", c.ID, Command{Type: "message", Text: "more"}), "capacity")
	for _, run := range []*testExecution{first, second, third} {
		run.result <- ExecutionResult{}
	}
}

func TestSurfaceLeaseHeartbeatBoundedByTurnDeadline(t *testing.T) {
	e := newExecutor()
	opts := options(t, e)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	opts.Now = func() time.Time { return time.Unix(0, clock.Load()) }
	opts.Surfaces[0].LeaseSeconds, opts.Surfaces[0].MaxTurnSeconds = 2, 3
	s := openTest(t, opts)
	session := createTest(t, s, "alice")
	commandTest(t, s, "alice", session.ID, Command{Type: "message", Text: "work"})
	run := receiveRun(t, e)
	current, _ := s.Session("alice", session.ID)
	if current.Lease.ExpiresAt.Sub(opts.Now()) != 2*time.Second {
		t.Fatalf("lease = %+v", current.Lease)
	}
	clock.Add(int64(time.Second))
	if err := run.req.Heartbeat(); err != nil {
		t.Fatal(err)
	}
	clock.Add(int64(time.Second))
	if err := run.req.Heartbeat(); err != nil {
		t.Fatal(err)
	}
	current, _ = s.Session("alice", session.ID)
	if current.Lease.ExpiresAt.Sub(opts.Now()) != time.Second {
		t.Fatal("heartbeat extended lease beyond configured turn deadline")
	}
	clock.Add(int64(time.Second))
	requireCode(t, run.req.Heartbeat(), "stale_lease")
	requireCode(t, s.CheckExecution("alice", session.ID, run.req.TurnID, run.req.LeaseID, run.req.Fence), "stale_lease")
	s.SweepLeases()
	waitStatus(t, s, "alice", session.ID, "recovery_required")
}

func TestSurfaceTurnTimeoutCancelsActualExecutor(t *testing.T) {
	e := newExecutor()
	opts := options(t, e)
	opts.Surfaces[0].MaxTurnSeconds = 1
	s := openTest(t, opts)
	session := createTest(t, s, "alice")
	commandTest(t, s, "alice", session.ID, Command{Type: "message", Text: "wait"})
	run := receiveRun(t, e)
	select {
	case <-run.done:
	case <-time.After(2 * time.Second):
		t.Fatal("configured turn timeout did not cancel executor")
	}
	waitStatus(t, s, "alice", session.ID, "recovery_required")
}

func TestSurfaceReloadFencesChangedPolicyAndRetainsCustody(t *testing.T) {
	for _, change := range []string{"disabled", "instructions", "authority-revision", "removed"} {
		t.Run(change, func(t *testing.T) {
			e := newExecutor()
			e.delayStop = make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(e.delayStop) }) }
			defer release()
			opts := options(t, e)
			opts.MaxActive = 1
			opts.MaxActivePerOwner = 1
			var hooks []string
			var s *Service
			opts.OnStop = func(_ context.Context, owner, sessionID string) error {
				current, _ := s.Session(owner, sessionID)
				if (current.Status != "recovery_required" && current.Status != "stopping") || !current.StopFencePending {
					return errors.New("external fence preceded session fence")
				}
				hooks = append(hooks, sessionID)
				return nil
			}
			s = openTest(t, opts)
			session := createTest(t, s, "alice")
			idle := createTest(t, s, "bob") // An idle Helper may own an external native run.
			commandTest(t, s, "alice", session.ID, Command{Type: "message", Text: "work"})
			run := receiveRun(t, e)
			next := clone(opts.Surfaces)
			switch change {
			case "disabled":
				next[0].Enabled = false
			case "instructions":
				next[0].Instructions = "new instructions"
			case "authority-revision":
				next[0].ConfigRevision = "authority-mapping-v2"
			case "removed":
				next = next[1:]
			}
			if err := s.ReloadSurfaces(next); err != nil {
				t.Fatal(err)
			}
			if len(hooks) != 2 {
				t.Fatalf("active and idle external owners were not fenced: %v", hooks)
			}
			fenced, _ := s.Session("alice", session.ID)
			if fenced.Fence <= run.req.Fence || !fenced.CustodyPending || fenced.Lease.Status != "policy_fenced" || fenced.ConfigDigest != session.ConfigDigest {
				t.Fatalf("custody released before actual writer stopped: %+v", fenced)
			}
			if err := s.CheckControl("bob", idle.ID); err == nil {
				t.Fatal("old idle session can still apply actions")
			}
			requireCode(t, s.CheckExecution("alice", session.ID, run.req.TurnID, run.req.LeaseID, run.req.Fence), "stale_fence")
			requireCode(t, run.req.Heartbeat(), "stale_lease")
			if err := run.emit(ExecutionEvent{Type: "tool.completed", Data: map[string]any{"name": "late-effect"}}); err == nil {
				t.Fatal("fenced output admitted")
			}
			requireCode(t, attemptCommand(s, "alice", session.ID, Command{Type: "resume"}), "surface_changed")
			helper, err := s.Create("charlie", CreateRequest{CommandID: "other-surface", SurfaceID: "help"})
			if err != nil {
				t.Fatal(err)
			}
			requireCode(t, attemptCommand(s, "charlie", helper.ID, Command{Type: "message", Text: "more"}), "capacity")
			release()
			<-run.done
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				fenced, _ = s.Session("alice", session.ID)
				if !fenced.CustodyPending {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if fenced.CustodyPending || fenced.Lease.Status != "released" || fenced.Status != "recovery_required" {
				t.Fatalf("writer return did not release custody safely: %+v", fenced)
			}
			commandTest(t, s, "charlie", helper.ID, Command{Type: "message", Text: "now admitted"})
			receiveRun(t, e).result <- ExecutionResult{}
			commandTest(t, s, "alice", session.ID, Command{Type: "stop"})
			waitStatus(t, s, "alice", session.ID, "stopped")
		})
	}
}

func TestInvalidReloadLeavesLastGoodAndRunningAttempt(t *testing.T) {
	e := newExecutor()
	opts := options(t, e)
	s := openTest(t, opts)
	session := createTest(t, s, "alice")
	commandTest(t, s, "alice", session.ID, Command{Type: "message", Text: "work"})
	run := receiveRun(t, e)
	invalid := []func(*Surface){func(p *Surface) { p.ExecutionModes = []string{} }, func(p *Surface) { p.LeaseSeconds = -1 }, func(p *Surface) { p.MaxActive = 1; p.MaxActivePerOwner = 2 }, func(p *Surface) { p.MaxTurnSeconds = 86401 }, func(p *Surface) { p.AuthorityRefs = []string{"a", "a"} }}
	for _, mutate := range invalid {
		next := clone(opts.Surfaces)
		mutate(&next[0])
		if s.ReloadSurfaces(next) == nil {
			t.Fatal("invalid policy was accepted")
		}
		if err := s.CheckExecution("alice", session.ID, run.req.TurnID, run.req.LeaseID, run.req.Fence); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ReloadSurfaces(opts.Surfaces); err != nil {
		t.Fatal(err)
	}
	if err := run.req.Heartbeat(); err != nil {
		t.Fatal("unchanged reload fenced running attempt", err)
	}
	run.result <- ExecutionResult{}
}

func TestReloadHookFailureRetainsConfigurationButNeverRestoresAuthority(t *testing.T) {
	opts := options(t, newExecutor())
	var fail atomic.Bool
	fail.Store(true)
	opts.OnStop = func(context.Context, string, string) error {
		if fail.Load() {
			return errors.New("external fence unavailable")
		}
		return nil
	}
	s := openTest(t, opts)
	session := createTest(t, s, "alice")
	next := clone(opts.Surfaces)
	next[0].Instructions = "changed"
	if err := s.ReloadSurfaces(next); err == nil {
		t.Fatal("failed fence reported a successful reload")
	}
	current, _ := s.Session("alice", session.ID)
	if !current.StopFencePending || current.Status != "recovery_required" {
		t.Fatal("failed external fence lost pending state")
	}
	requireCode(t, attemptCommand(s, "alice", session.ID, Command{Type: "resume"}), "invalid_state")
	fresh := createTest(t, s, "bob")
	if fresh.ConfigDigest != session.ConfigDigest {
		t.Fatal("failed reload replaced last good configuration")
	}
	fail.Store(false)
	if err := s.ReloadSurfaces(next); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckControl("bob", fresh.ID); err == nil {
		t.Fatal("old config binding silently changed")
	}
}
