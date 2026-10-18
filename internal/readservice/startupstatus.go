package readservice

import "time"

// StartupStatus identifies the operation currently blocking daemon readiness.
// This detailed form is served through the authenticated versioned health API.
type StartupStatus struct {
	Phase             string                    `json:"phase"`
	Target            string                    `json:"target,omitempty"`
	Since             time.Time                 `json:"since"`
	ElapsedSeconds    float64                   `json:"elapsedSeconds,omitempty"`
	WorktreeCount     int                       `json:"worktreeCount,omitempty"`
	RecoveryRunCount  int                       `json:"recoveryRunCount,omitempty"`
	AccumulationCount int                       `json:"accumulationCount,omitempty"`
	BudgetSeconds     float64                   `json:"budgetSeconds,omitempty"`
	BudgetUsedPercent float64                   `json:"budgetUsedPercent,omitempty"`
	BudgetState       string                    `json:"budgetState,omitempty"`
	BlockingCandidate *StartupRecoveryCandidate `json:"blockingCandidate,omitempty"`
}

// StartupRecoveryCandidate is the crash-resume candidate currently blocking startup.
type StartupRecoveryCandidate struct {
	Progress        StartupRecoveryProgress `json:"progress"`
	RunID           string                  `json:"runId,omitempty"`
	Gaggle          string                  `json:"gaggle,omitempty"`
	Workflow        string                  `json:"workflow,omitempty"`
	Disposition     string                  `json:"disposition,omitempty"`
	Phase           string                  `json:"phase,omitempty"`
	Operation       string                  `json:"operation,omitempty"`
	StartedAt       time.Time               `json:"startedAt,omitempty"`
	LastProgressAt  time.Time               `json:"lastProgressAt,omitempty"`
	ElapsedSeconds  float64                 `json:"elapsedSeconds,omitempty"`
	ProgressAgeSecs float64                 `json:"progressAgeSeconds,omitempty"`
}

// StartupRecoveryProgress is the aggregate crash-resume pass position.
type StartupRecoveryProgress struct {
	Total      int `json:"total"`
	Examined   int `json:"examined"`
	Resumed    int `json:"resumed"`
	Reattached int `json:"reattached"`
	Terminal   int `json:"terminal"`
	Skipped    int `json:"skipped"`
}

// AttachStartupStatus supplies the daemon's current startup operation.
func (s *Local) AttachStartupStatus(status func() *StartupStatus) {
	s.startupStatus = status
}

func (s *Local) startupStatusSnapshot() *StartupStatus {
	if s.startupStatus == nil {
		return nil
	}
	status := s.startupStatus()
	if status == nil {
		return nil
	}
	copy := *status
	if status.BlockingCandidate != nil {
		candidate := *status.BlockingCandidate
		copy.BlockingCandidate = &candidate
	}
	return &copy
}
