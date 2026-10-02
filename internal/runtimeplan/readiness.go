package runtimeplan

import "github.com/goobers/goobers/internal/credreadiness"

// CredentialCheck is metadata observed by the reporting process, never proof
// that a daemon or worker can resolve or authenticate with the source.
type CredentialCheck struct {
	Stage      string                   `json:"stage"`
	Capability string                   `json:"capability"`
	Kind       credreadiness.SourceKind `json:"kind"`
	Name       string                   `json:"name,omitempty"`
	Code       string                   `json:"code"`
	Outcome    string                   `json:"outcome"`
	Detail     string                   `json:"detail"`
	Process    Process                  `json:"process"`
	Source     Source                   `json:"source"`
}

// CredentialObservation separates presence from authentication and target
// identity, retaining stable codes even when source metadata is unavailable.
func CredentialObservation(stage string, check credreadiness.Check, process Process) CredentialCheck {
	fidelity := "observed"
	if check.Status == credreadiness.StatusUnobservable || check.Status == credreadiness.StatusUnsupportedSource {
		fidelity = "unobservable"
	}
	outcome := string(check.Status)
	if check.Status == credreadiness.StatusUnsupportedSource {
		outcome = "unsupported"
	}
	return CredentialCheck{Stage: stage, Capability: check.Name, Kind: check.Kind, Name: check.Source,
		Code: "credential_source_" + string(check.Status), Outcome: outcome, Detail: check.Detail, Process: process,
		Source: Source{fidelity, "reporting-process source metadata only; target daemon/worker availability unobservable"}}
}
