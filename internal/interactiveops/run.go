package interactiveops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/lock"
	"github.com/goobers/goobers/internal/platform/proc"
	"github.com/goobers/goobers/internal/sqliteuri"
	_ "modernc.org/sqlite"
)

func readRuns(root string) ([]Run, error) {
	dirs, err := instance.NewLayout(root).RunDirs()
	if err != nil {
		return nil, err
	}
	runs := []Run{}
	for _, runsDir := range dirs {
		entries, err := os.ReadDir(runsDir)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			dir := filepath.Join(runsDir, entry.Name())
			reader, err := journal.OpenReadOnly(dir)
			if err != nil {
				if errors.Is(err, journal.ErrNotRunDirectory) {
					continue
				}
				return nil, err
			}
			id, err := reader.Identity()
			if err != nil {
				return nil, err
			}
			phase, err := reader.Phase()
			if err != nil {
				return nil, err
			}
			state, _ := reader.State()
			runs = append(runs, Run{ID: id.RunID, Workflow: id.Workflow, Gaggle: id.Gaggle, Phase: string(phase), Reason: state.Reason, JournalPath: dir, StartedAt: id.StartedAt})
		}
	}
	return runs, nil
}
func nativeReceipt(dir string, a *Action) (state, runID, reason string) {
	path := filepath.Join(dir, "instance", "scheduler", "accepted-triggers.db")
	if _, err := os.Stat(path); err != nil {
		return "", "", ""
	}
	db, err := sql.Open("sqlite", sqliteuri.File(path)+"?mode=ro&_pragma=busy_timeout(100)")
	if err != nil {
		return "", "", ""
	}
	defer db.Close()
	_ = db.QueryRow("SELECT state,run_id,reason FROM triggers WHERE key=? AND actor=?", a.OperationID, "standalone-cli").Scan(&state, &runID, &reason)
	return
}
func (s *Service) reconcile(dir string, a *Action, runs []Run) {
	if a.Kind != "submit_run" || (a.Status != "running" && a.Status != "admitted" && a.Status != "uncertain") {
		return
	}
	prior := *a
	state, runID, reason := nativeReceipt(dir, a)
	if runID != "" {
		a.RunID = runID
	}
	if state == "rejected" {
		a.Status = "failed"
		a.OperationStatus = "failed"
		a.Error = reason
	}
	for _, run := range runs {
		if run.ID != a.RunID {
			continue
		}
		switch journal.RunPhase(run.Phase) {
		case journal.PhaseCompleted:
			a.Status = "succeeded"
			a.OperationStatus = "confirmed"
			a.Error = ""
		case journal.PhaseFailed, journal.PhaseEscalated:
			a.Status = "failed"
			a.OperationStatus = "confirmed"
			a.Error = run.Reason
		case journal.PhaseAborted:
			a.Status = "cancelled"
			a.OperationStatus = "confirmed"
			a.Error = run.Reason
		}
	}
	if a.Status == "running" || a.Status == "admitted" {
		held, err := lock.TryAcquire(filepath.Join(dir, "worker.lock"))
		if err == nil {
			_ = held.Release()
			a.Status = "uncertain"
			a.OperationStatus = "outcome_unknown"
			a.Error = "Worker custody is absent. The native submission may have committed; inspect its receipt/journal before recovery. This operation will not be dispatched again."
		}
	}
	if prior.Status != a.Status || prior.RunID != a.RunID || prior.Error != a.Error {
		_ = saveAction(dir, a)
	}
}
func (s *Service) startRun(scope Scope, dir string, a *Action) error {
	worker, err := lock.TryAcquire(filepath.Join(dir, "worker.lock"))
	for retry := 0; errors.Is(err, lock.ErrHeld) && retry < 20; retry++ {
		time.Sleep(10 * time.Millisecond)
		worker, err = lock.TryAcquire(filepath.Join(dir, "worker.lock"))
	}
	if err != nil {
		return errors.New("session helper worker capacity is occupied")
	}
	if _, err = os.Stat(s.opts.Binary); err != nil {
		_ = worker.Release()
		return fmt.Errorf("native binary unavailable: %w", err)
	}
	a.Status = "running"
	a.OperationStatus = "dispatched"
	a.Output = "Native scheduler submission dispatched; awaiting durable admission receipt and observed journal result."
	if err = saveAction(dir, a); err != nil {
		_ = worker.Release()
		return err
	}
	go s.execute(scope, dir, a.ID, worker)
	return nil
}

type boundedOutput struct {
	mu   sync.Mutex
	data []byte
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	remaining := 65536 - len(b.data)
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		b.data = append(b.data, p...)
	}
	return n, nil
}
func (b *boundedOutput) String() string { b.mu.Lock(); defer b.mu.Unlock(); return string(b.data) }
func (s *Service) execute(scope Scope, dir, id string, worker *lock.Handle) {
	defer worker.Release()
	a, err := readAction(dir, id)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.opts.RunTimeout)
	defer cancel()
	root := filepath.Join(dir, "instance")
	cmd := exec.Command(s.opts.Binary, "run", "--no-api", "--request-id", a.OperationID, a.Request.Gaggle+"/"+a.Request.Workflow, root)
	// Private HOME prevents discovery of developer CLI credentials. Only a
	// nonsecret executable search path is inherited; no process environment token
	// or ambient provider identity is passed to Goobers or its child stages.
	home := filepath.Join(dir, "home")
	_ = os.MkdirAll(home, 0700)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + os.TempDir(), "NO_COLOR=1"}
	cmd.Dir = root
	output := &boundedOutput{}
	cmd.Stdout = output
	cmd.Stderr = output
	type executionOutcome struct {
		err     error
		cleanup bool
	}
	done := make(chan executionOutcome, 1)
	tree, startErr := proc.Start(cmd)
	if startErr != nil {
		done <- executionOutcome{err: startErr, cleanup: true}
	} else {
		wait := make(chan error, 1)
		go func() { wait <- cmd.Wait() }()
		go func() {
			var cleanupErr error
			stopTree := func() error {
				cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancelCleanup()
				cleanupErr = tree.StopAndWait(cleanupCtx)
				return cleanupErr
			}
			outcome := proc.WaitOrKill(ctx, tree, wait, proc.WaitOptions{KillWait: 5 * time.Second, StopTree: stopTree})
			if !outcome.Canceled && !outcome.TimedOut {
				_ = stopTree()
			}
			if outcome.GaveUp || cleanupErr != nil {
				done <- executionOutcome{err: fmt.Errorf("process cleanup unconfirmed: %w", errors.Join(outcome.Err, cleanupErr)), cleanup: false}
			} else {
				done <- executionOutcome{err: outcome.Err, cleanup: true}
			}
		}()
	}
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case outcome := <-done:
			runErr := outcome.err
			for i := 0; i < 100; i++ {
				held, err := lock.TryAcquire(filepath.Join(dir, "operations.lock"))
				if err != nil {
					time.Sleep(10 * time.Millisecond)
					continue
				}
				current, err := readAction(dir, id)
				if err == nil {
					current.Output = output.String()
					current.CleanupConfirmed = outcome.cleanup
					runs, _ := readRuns(root)
					s.reconcile(dir, &current, runs)
					if current.Status == "running" || current.Status == "admitted" {
						current.Status = "uncertain"
						current.OperationStatus = "outcome_unknown"
						current.Error = "Native process exited without an observed terminal journal; do not retry the effect blindly."
						if runErr != nil {
							current.Error += " " + runErr.Error()
						}
					}
					_ = saveAction(dir, &current)
				}
				_ = held.Release()
				return
			}
			return
		case <-ticker.C:
			held, err := lock.TryAcquire(filepath.Join(dir, "operations.lock"))
			if err != nil {
				continue
			}
			current, err := readAction(dir, id)
			if err == nil {
				if current.StopRequested {
					cancel()
				}
				current.Output = output.String()
				_, runID, _ := nativeReceipt(dir, &current)
				if runID != "" {
					current.RunID = runID
				}
				_ = saveAction(dir, &current)
			}
			_ = held.Release()
		}
	}
}
func (s *Service) requestStop(dir, target string) error {
	actions, err := allActions(dir)
	if err != nil {
		return err
	}
	for _, a := range actions {
		if a.Kind == "submit_run" && (a.ID == target || a.RunID == target) && (a.Status == "running" || a.Status == "admitted") {
			a.StopRequested = true
			return saveAction(dir, &a)
		}
	}
	return errors.New("owned active run not found")
}
