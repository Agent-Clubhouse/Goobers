package interactiveops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/platform/lock"
)

type Service struct{ opts Options }

func New(opts Options) (*Service, error) {
	if opts.Root == "" {
		return nil, errors.New("helper operations root is required")
	}
	var err error
	opts.Root, err = filepath.Abs(opts.Root)
	if err != nil {
		return nil, err
	}
	if opts.RunTimeout <= 0 {
		opts.RunTimeout = 60 * time.Second
	}
	if opts.RunTimeout > 5*time.Minute {
		return nil, errors.New("prototype run timeout exceeds five minutes")
	}
	if err := os.MkdirAll(opts.Root, 0700); err != nil {
		return nil, err
	}
	return &Service{opts: opts}, nil
}
func digest(b []byte) string { x := sha256.Sum256(b); return hex.EncodeToString(x[:]) }
func validScope(scope Scope) error {
	if strings.TrimSpace(scope.ActorID) == "" || strings.TrimSpace(scope.SessionID) == "" || len(scope.ActorID) > 256 || len(scope.SessionID) > 256 {
		return errors.New("authenticated actor and session are required")
	}
	return nil
}
func (s *Service) scopeDir(scope Scope) string {
	return filepath.Join(s.opts.Root, digest([]byte(scope.ActorID+"\x00"+scope.SessionID)))
}
func (s *Service) locked(scope Scope) (string, func(), error) {
	if err := validScope(scope); err != nil {
		return "", nil, err
	}
	dir := s.scopeDir(scope)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", nil, err
	}
	l, err := lock.TryAcquire(filepath.Join(dir, "operations.lock"))
	for retry := 0; errors.Is(err, lock.ErrHeld) && retry < 20; retry++ {
		time.Sleep(10 * time.Millisecond)
		l, err = lock.TryAcquire(filepath.Join(dir, "operations.lock"))
	}
	if err != nil {
		return "", nil, fmt.Errorf("helper busy: %w", err)
	}
	release := func() { _ = l.Release() }
	if err = s.ensure(scope, dir); err != nil {
		release()
		return "", nil, err
	}
	return dir, release, nil
}
func writeJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, b)
}
func atomicWrite(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".pending-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}
func actionPath(dir, id string) string { return filepath.Join(dir, "actions", id+".json") }
func saveAction(dir string, a *Action) error {
	a.UpdatedAt = time.Now().UTC()
	return writeJSON(actionPath(dir, a.ID), a)
}
func readAction(dir, id string) (Action, error) {
	if len(id) != 35 || !strings.HasPrefix(id, "op_") {
		return Action{}, errors.New("unknown action")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(id, "op_")); err != nil {
		return Action{}, errors.New("unknown action")
	}
	b, err := os.ReadFile(actionPath(dir, id))
	if err != nil {
		return Action{}, err
	}
	var a Action
	err = json.Unmarshal(b, &a)
	return a, err
}
func allActions(dir string) ([]Action, error) {
	entries, err := os.ReadDir(filepath.Join(dir, "actions"))
	if err != nil {
		return nil, err
	}
	out := []Action{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		a, err := readAction(dir, strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}
func configDigest(dir string) (string, error) {
	hash := sha256.New()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("symlinks are not permitted in fixture configuration")
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return errors.New("nonregular configuration file")
		}
		rel, _ := filepath.Rel(dir, path)
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(hash, "%s\x00%d\x00", rel, len(b))
		_, err = hash.Write(b)
		return err
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
func (s *Service) Snapshot(ctx context.Context, scope Scope) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	dir, release, err := s.locked(scope)
	if err != nil {
		return State{}, err
	}
	defer release()
	root := filepath.Join(dir, "instance")
	set, _, err := instance.LoadConfigDir(filepath.Join(root, "config"))
	if err != nil {
		return State{}, err
	}
	actions, err := allActions(dir)
	if err != nil {
		return State{}, err
	}
	runs, err := readRuns(root)
	if err != nil {
		return State{}, err
	}
	for i := range actions {
		s.reconcile(dir, &actions[i], runs)
	}
	repo, err := s.repository(scope, dir)
	if err != nil {
		return State{}, err
	}
	dg, err := configDigest(filepath.Join(root, "config"))
	if err != nil {
		return State{}, err
	}
	return State{Mode: "isolated-local-fixture", ActorID: scope.ActorID, SessionID: scope.SessionID, InstanceRoot: root, Repository: repo, ConfigDigest: dg, Gaggles: set.Gaggles, Workflows: set.Workflows, Runs: runs, Actions: actions, Backlog: readBacklog(repo), WorkflowDraft: workflowDraft("invoice-check-custom", repo), Capabilities: []string{"read configured gaggle/workflow/run state", "propose/apply gaggle and workflow displayName/enabled", "author/add validated bounded local workflows", "submit real Goobers workflow and observe journal", "request stop of an owned run"}, Limitations: []string{"local fixture provider; no live ADO/Azure authority", "deterministic workflow steering unsupported; steer the interactive workspace session instead", "one active helper run per session; bounded native process; no cloud-scale claim", "configuration applies only to this isolated session fixture"}}, nil
}
func (s *Service) Propose(ctx context.Context, scope Scope, req Request) (Action, error) {
	if err := ctx.Err(); err != nil {
		return Action{}, err
	}
	if req.IdempotencyKey == "" || len(req.IdempotencyKey) > 128 {
		return Action{}, errors.New("idempotencyKey must contain 1–128 characters")
	}
	dir, release, err := s.locked(scope)
	if err != nil {
		return Action{}, err
	}
	defer release()
	if err = mutationAllowed(dir); err != nil {
		return Action{}, err
	}
	if req.Gaggle == "" {
		req.Gaggle = "interactive"
	}
	payload, _ := json.Marshal(req)
	id := "op_" + digest([]byte(scope.ActorID + "\x00" + scope.SessionID + "\x00" + req.IdempotencyKey))[:32]
	if prior, err := readAction(dir, id); err == nil {
		if prior.PayloadDigest != digest(payload) {
			return Action{}, errors.New("idempotency key already binds a different request")
		}
		return prior, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Action{}, err
	}
	root := filepath.Join(dir, "instance")
	dg, err := configDigest(filepath.Join(root, "config"))
	if err != nil {
		return Action{}, err
	}
	summary, preview, err := s.preview(scope, dir, req)
	if err != nil {
		return Action{}, err
	}
	now := time.Now().UTC()
	a := Action{ID: id, OperationID: id, ActorID: scope.ActorID, SessionID: scope.SessionID, Kind: req.Kind, Status: "proposed", OperationStatus: "prepared", Summary: summary, Preview: preview, ConfigDigest: dg, PayloadDigest: digest(payload), Request: req, CreatedAt: now, UpdatedAt: now}
	return a, saveAction(dir, &a)
}
func (s *Service) Get(ctx context.Context, scope Scope, id string) (Action, error) {
	if err := ctx.Err(); err != nil {
		return Action{}, err
	}
	dir, release, err := s.locked(scope)
	if err != nil {
		return Action{}, err
	}
	defer release()
	a, err := readAction(dir, id)
	if err != nil {
		return a, err
	}
	runs, err := readRuns(filepath.Join(dir, "instance"))
	if err != nil {
		return a, err
	}
	s.reconcile(dir, &a, runs)
	return a, nil
}
func (s *Service) Apply(ctx context.Context, scope Scope, id string) (Action, error) {
	if err := ctx.Err(); err != nil {
		return Action{}, err
	}
	dir, release, err := s.locked(scope)
	if err != nil {
		return Action{}, err
	}
	defer release()
	if err = mutationAllowed(dir); err != nil {
		return Action{}, err
	}
	a, err := readAction(dir, id)
	if err != nil {
		return a, err
	}
	if a.ActorID != scope.ActorID || a.SessionID != scope.SessionID {
		return Action{}, errors.New("action owner mismatch")
	}
	if a.Status != "proposed" {
		return a, nil
	} // including uncertain: never blind retry.
	root := filepath.Join(dir, "instance")
	dg, err := configDigest(filepath.Join(root, "config"))
	if err != nil {
		return a, err
	}
	if a.ConfigDigest != dg {
		return a, errors.New("configuration changed since preview; create a fresh proposal")
	}
	if _, _, err = s.preview(scope, dir, a.Request); err != nil {
		return a, err
	}
	now := time.Now().UTC()
	a.AuthorizedAt = &now
	a.AuthorizedBy = scope.ActorID
	a.Status = "authorized"
	if err = saveAction(dir, &a); err != nil {
		return a, err
	}
	switch a.Kind {
	case "tweak_gaggle", "tweak_workflow", "add_workflow":
		a.OperationStatus = "dispatched"
		a.Status = "running"
		if err = saveAction(dir, &a); err != nil {
			return a, err
		}
		err = s.applyConfig(scope, dir, a.Request)
		if err != nil {
			a.Status = "uncertain"
			a.OperationStatus = "outcome_unknown"
			a.Error = err.Error()
		} else {
			a.Status = "succeeded"
			a.OperationStatus = "confirmed"
			a.Output = "Native Goobers configuration validation passed; durable config transaction committed."
		}
	case "submit_run":
		if err = s.startRun(scope, dir, &a); err != nil {
			a.Status = "failed"
			a.OperationStatus = "failed"
			a.Error = err.Error()
		}
	case "stop_run":
		err = s.requestStop(dir, a.Request.RunID)
		if err != nil {
			a.Status = "failed"
			a.OperationStatus = "failed"
			a.Error = err.Error()
		} else {
			a.Status = "succeeded"
			a.OperationStatus = "confirmed"
			a.Output = "Stop request recorded. Run completion is pending until a terminal native journal phase is observed."
		}
	}
	return a, saveAction(dir, &a)
}

// StopSession fences further mutations, records stop intent, then waits for
// the owned worker to confirm process-tree cleanup. Disconnect is not stop.
func (s *Service) StopSession(ctx context.Context, scope Scope) error {
	dir, release, err := s.locked(scope)
	if err != nil {
		return err
	}
	if err = atomicWrite(filepath.Join(dir, "stopped"), []byte("explicit stop\n")); err != nil {
		release()
		return err
	}
	actions, err := allActions(dir)
	if err != nil {
		release()
		return err
	}
	for _, a := range actions {
		if a.Kind == "submit_run" && (a.Status == "admitted" || a.Status == "running" || a.Status == "uncertain") {
			a.StopRequested = true
			if err = saveAction(dir, &a); err != nil {
				release()
				return err
			}
		}
	}
	release()
	bounded, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	for {
		held, lockErr := lock.TryAcquire(filepath.Join(dir, "worker.lock"))
		if lockErr == nil {
			_ = held.Release()
			break
		}
		if !errors.Is(lockErr, lock.ErrHeld) {
			return lockErr
		}
		select {
		case <-bounded.Done():
			return fmt.Errorf("helper stop pending: %w", bounded.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	dir, release, err = s.locked(scope)
	if err != nil {
		return err
	}
	defer release()
	actions, err = allActions(dir)
	if err != nil {
		return err
	}
	for _, a := range actions {
		if a.Kind == "submit_run" && a.OperationStatus != "prepared" && a.OperationStatus != "failed" && !a.CleanupConfirmed {
			return errors.New("helper process-tree custody is unknown; stop cannot yet be confirmed")
		}
	}
	return nil
}
func mutationAllowed(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "stopped")); err == nil {
		return errors.New("session helper operations are stopped")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Close fences and stops all fixture scopes in this service root. It must be
// awaited before the host claims successful shutdown.
func (s *Service) Close(ctx context.Context) error {
	entries, err := os.ReadDir(s.opts.Root)
	if err != nil {
		return err
	}
	var failures []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.opts.Root, entry.Name(), "owner.json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			failures = append(failures, err)
			continue
		}
		var scope Scope
		if err = json.Unmarshal(b, &scope); err != nil {
			failures = append(failures, err)
			continue
		}
		if err = s.StopSession(ctx, scope); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
