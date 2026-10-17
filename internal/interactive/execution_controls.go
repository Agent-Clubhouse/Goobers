package interactive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
)

func (s *Service) executionModeAvailable(mode string) bool {
	for _, available := range s.opts.AvailableExecutionModes {
		if mode == available {
			return true
		}
	}
	return false
}

// ExecutionModes returns the configured host allowlist, not a claim that a
// particular model account or provider credential is currently available.
func (s *Service) ExecutionModes() []string {
	return append([]string{}, s.opts.AvailableExecutionModes...)
}

func workflowDigest(source string) string {
	if source == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(source))
	return hex.EncodeToString(sum[:])
}

// RequestChildFromExecution is a host-only tool bridge. Owner and attempt
// references MUST come from the authenticated ExecutionRequest, never model
// arguments. It authorizes and persists a proposal under one coordinator lock;
// it neither approves the proposal nor starts a child. A child execution cannot
// recursively propose another child. Replaying a tool key is permitted only
// with a current parent lease (including a verified replacement attempt), and
// never changes the original definition or consumes another child reservation.
func (s *Service) RequestChildFromExecution(ctx context.Context, owner, sessionID, turnID, leaseID string, fence uint64, key, prompt, workflowYAML string) (CommandReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkExecutionLocked(owner, sessionID, turnID, leaseID, fence); err != nil {
		return CommandReceipt{}, err
	}
	rec := s.state.Sessions[sessionID]
	run := s.active[sessionID][turnID]
	if run == nil || run.request.ChildID != "" || rec.Session.Status != "running" || rec.Session.ActiveTurnID != turnID {
		return CommandReceipt{}, problem(403, "child_depth_denied", "only the active parent execution may propose a child")
	}
	if !validCommandID(key) {
		return CommandReceipt{}, problem(400, "invalid", "a bounded stable host tool key is required")
	}
	// Namespace the logical tool intent by durable turn, not physical lease.
	// The current lease was checked above, but recovery must not spend another
	// child reservation when it retries the same logical request.
	commandID := "agent_" + digest(struct{ SessionID, TurnID, Key string }{sessionID, turnID, key})[:48]
	stableDigest := digest(struct{ Owner, SessionID, TurnID, Key, Prompt, WorkflowYAML string }{owner, sessionID, turnID, key, prompt, workflowYAML})

	cmd := Command{CommandID: commandID, Type: "child.request", Text: prompt, WorkflowYAML: workflowYAML, TurnID: turnID, ExpectedSequence: rec.Session.Sequence, ExpectedFence: fence}
	return s.commandLocked(ctx, owner, sessionID, cmd, commandOrigin{source: "execution", turnID: turnID, digest: stableDigest})
}
