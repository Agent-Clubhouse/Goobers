// Package runtimeplan describes execution inputs without running workload probes.
package runtimeplan

import (
	"os"
	"os/user"
	"runtime"
	"strconv"
)

// Source distinguishes process observations from configuration and limitations.
type Source struct {
	Fidelity string `json:"fidelity"`
	Detail   string `json:"detail"`
}

// Process is the identity of the process that actually built the report. UID
// and GID are effective IDs on Unix and the current token's SID on Windows.
// Environment variables are deliberately not accepted as identity evidence.
type Process struct {
	PID    int    `json:"pid"`
	OS     string `json:"os"`
	UID    string `json:"uid,omitempty"`
	GID    string `json:"gid,omitempty"`
	Source Source `json:"source"`
}

func ObserveProcess() Process {
	p := Process{PID: os.Getpid(), OS: runtime.GOOS, Source: Source{"observed", "current process OS and effective identity"}}
	if runtime.GOOS != "windows" {
		p.UID = strconv.Itoa(os.Geteuid())
		p.GID = strconv.Itoa(os.Getegid())
	} else if u, err := user.Current(); err == nil {
		p.UID, p.GID = u.Uid, u.Gid
	} else {
		p.Source = Source{"unobservable", "current process token identity unavailable"}
	}
	return p
}

// Identity records the evidence boundary for a selected execution location.
// A self runner means the daemon host, not the interactive CLI process. Neither
// a matching username nor a service-account environment variable proves that
// daemon/worker credentials, filesystem namespaces, or logon tokens were used.
type Identity struct {
	Outcome string `json:"outcome"`
	Code    string `json:"code"`
	Detail  string `json:"detail"`
	Source  Source `json:"source"`
}

func TargetIdentity(p Process, kind string) Identity {
	result := Identity{Outcome: "unsupported", Code: "target_identity_unobservable", Detail: "checks ran in the reporting process; target daemon/service identity and environment are unobservable; safe impersonation is unavailable", Source: Source{"unobservable", "no target-process attestation or safe impersonation"}}
	if p.UID == "" || p.Source.Fidelity != "observed" {
		result.Code = "process_identity_unobservable"
		return result
	}
	switch kind {
	case "image", "deployment":
		result.Code = "worker_identity_unobservable"
		result.Detail = "container/remote worker process, service account and security context were not observed; local checks cannot prove worker equivalence"
	case "self", "":
		result.Code = "daemon_identity_unobservable"
	default:
		result.Code = "runner_identity_unsupported"
	}
	return result
}
