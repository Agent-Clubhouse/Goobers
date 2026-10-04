package localscheduler

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sessioning"
)

func sessionAdmissionFixture(n int) journal.RunIdentity {
	id := fmt.Sprintf("%032x", n)
	digest := journal.Digest([]byte("session pin"))
	return journal.RunIdentity{RunID: id, Gaggle: "gaggle", Workflow: "interactive-session", WorkflowVersion: 1, ConfigGeneration: digest, WorkflowDigest: digest, GooberDigest: digest, Trigger: journal.Trigger{Kind: journal.TriggerSignal, Ref: "session:session:turn"}, Session: &journal.SessionLineage{Gaggle: "gaggle", SessionID: "session", TurnID: "turn", MessageID: "message", AcceptanceID: "trigger-" + id, EnvelopeDigest: digest, InputDigest: digest}}
}

func TestSessionAdmissionSharesGlobalCapacityWithoutCatalogOrWatchdogRelease(t *testing.T) {
	s, _ := newTestScheduler(t, nil)
	s.conditions.SetInstanceLimits(1, nil, nil)
	id := sessionAdmissionFixture(1)
	release, err := s.ReserveSession(t.Context(), id, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.workflows) != 0 {
		t.Fatal("session registered a fake workflow")
	}
	_, err = s.ReserveSession(t.Context(), sessionAdmissionFixture(2), time.Now())
	requireChildRefusal(t, err, ReasonInstanceMaxParallel)
	s.ReleaseRun(id.RunID, id.Workflow)
	s.ReleaseReconciled(id.RunID, id.Workflow)
	if s.conditions.totalActive != 1 {
		t.Fatal("terminal event released unjoined writer")
	}
	release()
	release()
	if s.conditions.totalActive != 0 {
		t.Fatal("custody release not exactly once")
	}
}

func TestSessionAdmissionConcurrentGaggleBoundAndExplicitTurnsIgnoreAutomationCadence(t *testing.T) {
	s, _ := newTestScheduler(t, nil)
	var wg sync.WaitGroup
	var mu sync.Mutex
	releases := []func(){}
	for n := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := s.ReserveSession(t.Context(), sessionAdmissionFixture(n+1), time.Now())
			if err == nil {
				mu.Lock()
				releases = append(releases, release)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(releases) != sessioning.MaxExecutingTurns {
		t.Fatal(len(releases))
	}
	for _, release := range releases {
		release()
	}
	for n := range 20 {
		release, err := s.ReserveSession(t.Context(), sessionAdmissionFixture(n+50), time.Now())
		if err != nil {
			t.Fatal("manual conversation inherited automation cadence", err)
		}
		release()
	}
}

func TestSessionAdmissionRestoresTerminalUnjoinedAndTransfersStartupSlot(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprint(terminal), func(t *testing.T) {
			id := sessionAdmissionFixture(1)
			runs := t.TempDir()
			run, err := journal.Create(runs, id, nil)
			if err != nil {
				t.Fatal(err)
			}
			if terminal {
				if err = run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)}); err != nil {
					t.Fatal(err)
				}
			}
			if err = run.Close(); err != nil {
				t.Fatal(err)
			}
			s, _ := newTestScheduler(t, nil)
			s.conditions.SetInstanceLimits(1, nil, nil)
			if err = s.ReconcileRunDirs([]string{runs}, []string{filepath.Join(runs, id.RunID)}, time.Now()); err != nil {
				t.Fatal(err)
			}
			s.ReleaseReconciled(id.RunID, id.Workflow)
			release, err := s.RestoreSession(id)
			if err != nil {
				t.Fatal(err)
			}
			if s.conditions.totalActive != 1 {
				t.Fatal("restored custody double counted or disappeared")
			}
			if _, err = s.RestoreSession(id); err == nil {
				t.Fatal("duplicate custody owner")
			}
			_, err = s.ReserveSession(t.Context(), sessionAdmissionFixture(2), time.Now())
			requireChildRefusal(t, err, ReasonInstanceMaxParallel)
			release()
			next, err := s.ReserveSession(t.Context(), sessionAdmissionFixture(2), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			release()
			if s.conditions.totalActive != 1 {
				t.Fatal("stale release stole later permit")
			}
			next()
		})
	}
}

func TestSessionAdmissionRefusesInvalidOrUnauditableIdentity(t *testing.T) {
	for _, mode := range []string{"missing", "foreign", "pin", "audit"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := newTestScheduler(t, nil)
			id := sessionAdmissionFixture(1)
			switch mode {
			case "missing":
				id.Session = nil
			case "foreign":
				id.Session.Gaggle = "foreign"
			case "pin":
				id.Session.InputDigest = strings.Repeat("x", 71)
			case "audit":
				if err := s.log.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.ReserveSession(t.Context(), id, time.Now()); err == nil {
				t.Fatal("admitted without verified source/audit")
			}
			if s.conditions.totalActive != 0 {
				t.Fatal("failed admission leaked slot")
			}
		})
	}
}
