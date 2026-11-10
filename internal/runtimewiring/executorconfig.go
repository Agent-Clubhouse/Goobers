package runtimewiring

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/handoffcheck"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/stageexec"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/workspacerevision"
	"github.com/goobers/goobers/internal/worktree"
)

// ExecutorConfig holds constructed stage executors and their shared workspace
// settings. It cannot start a workflow or construct a local runner. The local
// command adapts it to ExecutorConfig and adds daemon-only lifecycle callbacks.
type ExecutorConfig struct {
	ConfigGeneration          string
	RunControls               apiv1.RunControls
	NewDeterministic          stageexec.NewDeterministicFunc
	NewAgentic                stageexec.NewAgenticFunc
	NewAgenticWithMCPServers  stageexec.NewAgenticWithMCPServersFunc
	Automated                 invoke.Automated
	RunnersDeclared           bool
	Worktrees                 *worktree.Manager
	PinnedWorkspace           bool
	PinnedCleanPolicy         string
	BranchNamespaces          map[string]string
	ScratchDir                string
	RunsDir                   string
	HandoffSchemaLoader       func(string) (*handoffcheck.Schema, error)
	RepoCloneURL              func(apiv1.RepoRef) (string, error)
	AdditionalRepos           []apiv1.RepoRef
	ResolveRepositoryIdentity workspacerevision.RepositoryLookup
	GateGooberCapabilities    map[string][]string
	AgentProvenance           map[string]stageexec.AgentProvenance
	LookPathFunc              func(string) (string, error)
	SelfExecutionDenied       bool
	SelfExecutionObserved     func(bool)
	Telemetry                 stageexec.SpanStarter
	IssueOwnershipAssignees   string
	IssueOwnershipUnassigned  string
}

var _ stageexec.SpanStarter = (*telemetry.Client)(nil)
