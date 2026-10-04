package runner

import (
	"context"
	"errors"
	"reflect"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workspacerevision"
)

// StageRestartExecutionFactories replace every automation effect on a dedicated
// continuation driver. Worktrees and their repository locks remain shared.
type StageRestartExecutionFactories struct {
	NewDeterministic   NewDeterministicFunc
	NewAgentic         NewAgenticFunc
	Context            func(context.Context, journal.RunIdentity, SecretRegistrar) (context.Context, func(), error)
	RepositoryIdentity workspacerevision.RepositoryLookup
	GateCapabilities   map[string][]string
	AdditionalRepos    []apiv1.RepoRef
}

// ForChildStageRestartExecution retains the already-bound isolated factories
// while installing the human authority lifetime. It never accepts native
// replacement factories or changes the admitted child's capability ceiling.
func (r *Runner) ForChildStageRestartExecution(id journal.RunIdentity, begin func(context.Context, journal.RunIdentity, SecretRegistrar) (context.Context, func(), error)) (*Runner, error) {
	if r == nil || id.Child == nil {
		return nil, errors.New("runner: contained human child driver unavailable")
	}
	return r.ForStageRestartExecution(id, StageRestartExecutionFactories{NewAgentic: r.cfg.NewAgentic, NewDeterministic: r.cfg.NewDeterministic, Context: begin, RepositoryIdentity: r.cfg.ResolveRepositoryIdentity, GateCapabilities: r.cfg.GateGooberCapabilities})
}

// ForStageRestartExecution creates a driver which can only resume the exact
// human epoch. It cannot start ordinary automation or invoke automation hooks.
func (r *Runner) ForStageRestartExecution(id journal.RunIdentity, f StageRestartExecutionFactories) (*Runner, error) {
	if r == nil || id.RunID == "" || id.Session != nil || f.Context == nil || f.NewAgentic == nil || f.NewDeterministic == nil {
		return nil, errors.New("runner: human restart execution factories unavailable")
	}
	if id.Child != nil && (id.Child.ExecutionEpoch == 0 || id.ValidateChildLineage() != nil || r.cfg.childExecution == nil || r.cfg.childExecution.RunID != id.RunID || !reflect.DeepEqual(r.cfg.childExecution.Child, id.Child)) {
		return nil, errors.New("runner: human child restart requires its contained execution driver")
	}
	cfg := r.cfg
	cfg.ConfigGeneration = id.ConfigGeneration
	cfg.NewAgentic, cfg.NewDeterministic = f.NewAgentic, f.NewDeterministic
	cfg.StageRestartContext = func(ctx context.Context, actual journal.RunIdentity, reg SecretRegistrar) (context.Context, func(), error) {
		if actual.RunID != id.RunID || actual.Gaggle != id.Gaggle || actual.WorkflowDigest != id.WorkflowDigest || actual.GooberDigest != id.GooberDigest || actual.ConfigGeneration != id.ConfigGeneration || !reflect.DeepEqual(actual.Child, id.Child) {
			return nil, nil, errors.New("runner: human driver cannot resume another identity")
		}
		return f.Context(ctx, actual, reg)
	}
	cfg.stageRestartOnly = id.RunID
	cfg.ResolveRepositoryIdentity = f.RepositoryIdentity
	cfg.GateGooberCapabilities = f.GateCapabilities
	cfg.AdditionalRepos = append([]apiv1.RepoRef(nil), f.AdditionalRepos...)
	cfg.Escalation, cfg.ClaimedItems = nil, nil
	cfg.Blocked, cfg.Failed, cfg.ExistingFix = nil, nil, nil
	cfg.PrepareTerminal, cfg.FinalizeTerminal, cfg.NotifyTerminal = nil, nil, nil
	cfg.BaselineHealth, cfg.LookPathFunc = nil, nil
	cfg.ChildHandoff, cfg.ChildParentCapacity = nil, nil
	return New(cfg)
}
