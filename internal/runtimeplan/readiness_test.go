package runtimeplan

import (
	"testing"

	"github.com/goobers/goobers/internal/credreadiness"
)

func TestCredentialObservation(t *testing.T) {
	process := Process{
		PID: 42,
		OS:  "linux",
		UID: "1000",
		GID: "1001",
		Source: Source{
			Fidelity: "observed",
			Detail:   "fixture reporting process",
		},
	}
	for _, tt := range []struct {
		status   credreadiness.Status
		code     string
		outcome  string
		fidelity string
	}{
		{credreadiness.StatusUsable, "credential_source_usable", "usable", "observed"},
		{credreadiness.StatusEmpty, "credential_source_empty", "empty", "observed"},
		{credreadiness.StatusExpired, "credential_source_expired", "expired", "observed"},
		{credreadiness.StatusRefreshFailed, "credential_source_refresh_failed", "refresh_failed", "observed"},
		{credreadiness.StatusUnobservable, "credential_source_unobservable", "unobservable", "unobservable"},
		{credreadiness.StatusUnsupportedSource, "credential_source_unsupported_source", "unsupported", "unobservable"},
		{credreadiness.StatusPresent, "credential_source_present", "present", "observed"},
		{credreadiness.StatusAbsent, "credential_source_absent", "absent", "observed"},
	} {
		t.Run(string(tt.status), func(t *testing.T) {
			check := credreadiness.Check{
				Name:   "agent:model",
				Kind:   credreadiness.SourceFile,
				Source: "/fixture/credential",
				Status: tt.status,
				Detail: "fixture source metadata",
			}
			got := CredentialObservation("implement", check, process)
			want := CredentialCheck{
				Stage:      "implement",
				Capability: "agent:model",
				Kind:       credreadiness.SourceFile,
				Name:       "/fixture/credential",
				Code:       tt.code,
				Outcome:    tt.outcome,
				Detail:     "fixture source metadata",
				Process:    process,
				Source: Source{
					Fidelity: tt.fidelity,
					Detail:   "reporting-process source metadata only; target daemon/worker availability unobservable",
				},
			}
			if got != want {
				t.Fatalf("CredentialObservation() = %+v, want %+v", got, want)
			}
		})
	}
}
