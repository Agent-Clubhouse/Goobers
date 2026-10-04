package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sessioning"
)

// ForSessionExecution binds a dedicated driver to a real accepted session turn.
// Factories own the live human lease; automation provider/claim hooks are absent.
func (r *Runner) ForSessionExecution(id journal.RunIdentity, agent NewAgenticFunc) (*Runner, error) {
	if r == nil || id.Session == nil || agent == nil || id.EngineDriven() {
		return nil, errors.New("runner: session execution unavailable")
	}
	if err := id.ValidateSessionLineage(); err != nil {
		return nil, err
	}
	lineage := *id.Session
	id.Session = &lineage
	cfg := r.cfg
	cfg.sessionExecution = &id
	cfg.ConfigGeneration = id.ConfigGeneration
	cfg.NewAgentic = agent
	cfg.NewDeterministic = nil
	cfg.Escalation, cfg.ClaimedItems = nil, nil
	cfg.Blocked, cfg.Failed, cfg.ExistingFix = nil, nil, nil
	cfg.PrepareTerminal, cfg.FinalizeTerminal, cfg.NotifyTerminal = nil, nil, nil
	cfg.BaselineHealth, cfg.LookPathFunc = nil, nil
	cfg.ChildHandoff, cfg.ChildParentCapacity = nil, nil
	cfg.AdditionalRepos = nil
	cfg.GateGooberCapabilities = nil
	return New(cfg)
}

func (r *Runner) validateSessionStart(in StartInput) error {
	if in.SessionInputs == nil && r.cfg.sessionExecution == nil {
		return nil
	}
	if in.SessionInputs == nil || r.cfg.sessionExecution == nil {
		return errors.New("runner: session execution requires its dedicated driver")
	}
	if in.Child != nil || in.EventInputs != nil || in.Item != nil || in.ChildWorkspace != nil || in.ChildCredentials != nil || len(in.RequiredCapabilities) > 0 {
		return errors.New("runner: session cannot inherit other execution authority")
	}
	expected := r.cfg.sessionExecution
	if in.RunID != expected.RunID || in.Gaggle != expected.Gaggle || in.GooberDigest != expected.GooberDigest || in.Machine.Digest() != expected.WorkflowDigest {
		return errors.New("runner: session driver identity mismatch")
	}
	if len(in.Machine.Def.Spec.Tasks) != 1 || len(in.Machine.Def.Spec.Gates) != 0 || len(in.Machine.Def.Spec.Parallels) != 0 {
		return errors.New("runner: session machine must contain one agent turn")
	}
	task := in.Machine.Def.Spec.Tasks[0]
	if task.Type != apiv1.TaskAgentic || task.Workspace != apiv1.WorkspaceScratch || task.ChildWorkflows != nil || !reflect.DeepEqual(task.Capabilities, []string{"agent:model"}) {
		return errors.New("runner: session currently supports model-only scratch execution")
	}
	return nil
}

func prepareSessionInputs(in *StartInput, inputs map[string][]byte, grades map[string]apiv1.Integrity, scrubber journal.Scrubber) (*journal.SessionLineage, error) {
	if in.SessionInputs == nil {
		return nil, nil
	}
	manifest, err := in.SessionInputs.Validate(in.RunID, in.Gaggle)
	if err != nil {
		return nil, err
	}
	e := in.SessionInputs.Start
	if e.ConfigGeneration != in.configGeneration || e.GooberDigest != in.GooberDigest || in.Trigger.Kind != journal.TriggerSignal || in.Trigger.Ref != "session:"+e.SessionID+":"+e.TurnID {
		return nil, errors.New("runner: session input pins differ")
	}
	if !bytes.Equal(manifest, scrubber.Scrub(manifest)) {
		return nil, errors.New("runner: session input requires intake redaction")
	}
	inputs[sessioning.ContextInputName] = manifest
	grades[sessioning.ContextInputName] = apiv1.IntegrityUnapproved
	in.ContextPointers = append(in.ContextPointers, eventInputPointer(sessioning.ContextInputName, manifest, apiv1.IntegrityUnapproved))
	envelope, _ := json.Marshal(e)
	return &journal.SessionLineage{Gaggle: in.Gaggle, SessionID: e.SessionID, TurnID: e.TurnID, MessageID: e.MessageID, AcceptanceID: in.SessionInputs.AcceptanceID, EnvelopeDigest: journal.Digest(envelope), InputDigest: journal.Digest(manifest)}, nil
}

// refuseSessionResume runs before acquiring the mutable journal. Unknown
// invocation custody cannot be interpreted as permission for another process.
func (r *Runner) refuseSessionResume(runID string) error {
	rd, err := journal.OpenReadOnly(filepath.Join(r.cfg.RunsDir, runID))
	if err != nil {
		return err
	}
	id, err := rd.Identity()
	if err != nil {
		return err
	}
	if id.Session == nil && r.cfg.sessionExecution == nil {
		return nil
	}
	if id.Session == nil || r.cfg.sessionExecution == nil || !sameSessionIdentity(id, *r.cfg.sessionExecution) {
		return errors.New("runner: session resume requires exact accepted driver")
	}
	return nil
}

func sameSessionIdentity(a, b journal.RunIdentity) bool {
	return a.RunID == b.RunID && a.Gaggle == b.Gaggle && a.WorkflowDigest == b.WorkflowDigest && a.GooberDigest == b.GooberDigest && a.ConfigGeneration == b.ConfigGeneration && reflect.DeepEqual(a.Session, b.Session)
}

func (r *Runner) refuseSessionMutation(runID string) error {
	rd, err := journal.OpenReadOnly(filepath.Join(r.cfg.RunsDir, runID))
	if err != nil {
		return err
	}
	id, err := rd.Identity()
	if err != nil {
		return err
	}
	if id.Session != nil {
		return errors.New("runner: session retry requires a new accepted message")
	}
	return nil
}
