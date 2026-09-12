package readservice

import "time"

// StartupStatus identifies the operation currently blocking daemon readiness.
// This detailed form is served through the authenticated versioned health API.
type StartupStatus struct {
	Phase  string    `json:"phase"`
	Target string    `json:"target,omitempty"`
	Since  time.Time `json:"since"`
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
	return &copy
}
