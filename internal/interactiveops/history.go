package interactiveops

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
)

// SnapshotRecorded reads only an existing actor/session-owned execution record.
// It never invokes current policy/config/repository callbacks, initializes a
// scope, recovers a config transaction, reconciles an action, or starts a writer.
// The host must authorize historical session membership before calling it.
func (s *Service) SnapshotRecorded(ctx context.Context, scope Scope) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	dir, release, err := s.observed(scope)
	if err != nil {
		return State{}, err
	}
	defer release()
	root := filepath.Join(dir, "instance")
	set, err := loadNativeConfig(filepath.Join(root, "config"))
	if err != nil {
		return State{}, err
	}
	actions, err := allActions(dir)
	if err != nil {
		return State{}, err
	}
	runs, err := scopeRuns(dir)
	if err != nil {
		return State{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "repository-binding.json"))
	if err != nil {
		return State{}, err
	}
	var binding struct {
		Repository   string
		Capabilities []CommandCapability
		Policy       Policy
		ConfigDir    string
	}
	if err = json.Unmarshal(data, &binding); err != nil {
		return State{}, err
	}
	dg, err := configDigest(filepath.Join(root, "config"))
	if err != nil {
		return State{}, err
	}
	mode := "recorded-local-fixture"
	if binding.ConfigDir != "" {
		mode = "recorded-native-config"
	}
	return State{ReadOnly: true, Mode: mode, ActorID: scope.ActorID, SessionID: scope.SessionID, InstanceRoot: root, Repository: binding.Repository, ConfigDigest: dg, Policy: binding.Policy, Gaggles: set.Gaggles, Goobers: set.Goobers, Workflows: set.Workflows, Actions: actions, Runs: runs, Backlog: readBacklog(binding.Repository), CommandCapabilities: []CommandCapability{}, Capabilities: []string{"read recorded configuration, action receipts and native run journals"}, Limitations: []string{"historical snapshot; no current authority implied", "actions are recorded receipts; unresolved effects remain unresolved"}}, nil
}
