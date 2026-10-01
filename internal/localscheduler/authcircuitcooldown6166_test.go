package localscheduler

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/providers"
)

// switchableStarter returns a result that the test can change between runs.
type switchableStarter struct {
	mu     sync.Mutex
	starts int
	result StartResult
	err    error
}

func (s *switchableStarter) Start(context.Context, StartRequest) (StartResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.starts++
	return s.result, s.err
}

func (s *switchableStarter) set(result StartResult, err error) {
	s.mu.Lock()
	s.result, s.err = result, err
	s.mu.Unlock()
}

func (s *switchableStarter) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starts
}

// TestAuthCircuitReclosesAfterCooldown is #6166: a scheduled workflow whose
// run failed authentication once — a delivered token that expired mid-stage
// (github_auth_failed), or a token broker that failed while materializing a
// stage credential (credential_unavailable, returned alongside a start
// error) — stopped firing until the daemon restarted. The circuit must hold
// for a cooldown (#2687), then let the schedule fire again, back off further
// on a repeat failure, and close once a run gets past its credentials.
func TestAuthCircuitReclosesAfterCooldown(t *testing.T) {
	cases := map[string]struct {
		result StartResult
		err    error
	}{
		"expired delivered credential": {
			result: StartResult{
				Phase:          journal.PhaseFailed,
				FailureStage:   "queue-watch",
				FailureCode:    providers.ErrorCodeAuthFailed,
				FailureMessage: "delivered credential rejected (expired)",
			},
		},
		"credential broker failure": {
			result: StartResult{FailureCode: telemetry.ErrCodeCredentialUnavailable},
			err:    errors.New(`execute stage "update-behind-pr": materialize credential: get-access-token: exit status 1`),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			start := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
			clock := newFakeClock(start)
			starter := &switchableStarter{result: tc.result, err: tc.err}
			identity := WorkflowIdentity{Gaggle: "soak", Workflow: "merge-review"}
			sched, dir := newTestScheduler(t, []WorkflowEntry{{
				Workflow:  identity.Workflow,
				Gaggle:    identity.Gaggle,
				Schedules: []Schedule{fakeSchedule{d: 10 * time.Minute}},
				Starter:   starter,
			}}, WithClock(clock.Now, time.After))
			tick := func(at time.Time) {
				t.Helper()
				clock.now.Store(&at)
				sched.Tick(context.Background(), at)
				sched.Wait()
			}

			// The failing run opens the circuit.
			tick(start.Add(10 * time.Minute))
			if got := starter.count(); got != 1 {
				t.Fatalf("starts after first due tick = %d, want 1", got)
			}
			if !sched.authCircuitOpen(identity, clock.Now()) {
				t.Fatal("auth circuit did not open after the credential failure")
			}

			// Inside the cooldown the schedule is held (#2687).
			tick(start.Add(20 * time.Minute))
			if got := starter.count(); got != 1 {
				t.Fatalf("starts inside the cooldown = %d, want 1", got)
			}

			// After the cooldown the schedule fires again instead of
			// stalling until a restart.
			second := start.Add(10*time.Minute + authCircuitBaseCooldown)
			tick(second)
			if got := starter.count(); got != 2 {
				t.Fatalf("starts after the cooldown = %d, want 2 (schedule stalled behind the auth circuit)", got)
			}

			// The repeat failure re-opens it with a longer cooldown.
			tick(second.Add(authCircuitBaseCooldown))
			if got := starter.count(); got != 2 {
				t.Fatalf("starts inside the doubled cooldown = %d, want 2", got)
			}

			// Credentials recover: the half-open attempt succeeds and
			// closes the circuit, so the next due tick fires normally.
			starter.set(StartResult{Phase: journal.PhaseCompleted}, nil)
			third := second.Add(2 * authCircuitBaseCooldown)
			tick(third)
			if got := starter.count(); got != 3 {
				t.Fatalf("starts after the doubled cooldown = %d, want 3", got)
			}
			if _, tracked := sched.authCircuits[identity]; tracked {
				t.Fatal("successful run left the auth circuit's failure streak in place")
			}
			tick(third.Add(10 * time.Minute))
			if got := starter.count(); got != 4 {
				t.Fatalf("starts after recovery = %d, want 4", got)
			}

			// Every opening is journaled with its retry time, so a held
			// workflow is never silent.
			events, err := journal.ReadInstanceLog(dir)
			if err != nil {
				t.Fatal(err)
			}
			var held int
			for _, event := range events {
				if event.Type == journal.EventTickSkipped && strings.HasPrefix(event.Reason, ReasonProviderAuth+": retrying after ") {
					held++
				}
			}
			if held != 2 {
				t.Fatalf("auth-circuit tick.skipped events = %d, want 2: %+v", held, events)
			}
		})
	}
}

func TestAuthCircuitCooldownBacksOffToCap(t *testing.T) {
	for strikes, want := range map[int]time.Duration{
		1:  authCircuitBaseCooldown,
		2:  2 * authCircuitBaseCooldown,
		3:  4 * authCircuitBaseCooldown,
		4:  authCircuitMaxCooldown,
		50: authCircuitMaxCooldown,
	} {
		if got := authCircuitCooldown(strikes); got != want {
			t.Errorf("authCircuitCooldown(%d) = %v, want %v", strikes, got, want)
		}
	}
}

func TestAuthCircuitNextWakeupIncludesRetry(t *testing.T) {
	start := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	identity := WorkflowIdentity{Gaggle: "soak", Workflow: "merge-review"}
	sched, _ := newTestScheduler(t, []WorkflowEntry{{
		Workflow:  identity.Workflow,
		Gaggle:    identity.Gaggle,
		Schedules: []Schedule{neverSchedule{}},
		Starter:   &fakeStarter{},
	}}, WithClock(func() time.Time { return start }, time.After))
	sched.openAuthCircuit(identity, start)
	if got := sched.nextWakeup(start); got > authCircuitBaseCooldown {
		t.Fatalf("nextWakeup = %v, want <= the circuit's %v retry", got, authCircuitBaseCooldown)
	}
}
