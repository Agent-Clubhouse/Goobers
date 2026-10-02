// Command soak runs real workflows under externally isolated resource pressure.
package main

import "time"

// Profile is the versioned workload and resource contract shared with #1480.
// Resource limits are applied by the enclosing runtime, never by this driver.
type Profile struct {
	Name              string        `json:"name"`
	Runs              int           `json:"runs"`
	RampWindow        time.Duration `json:"rampWindow"`
	Duration          time.Duration `json:"duration"`
	CPULimit          float64       `json:"cpuLimit"`
	MemoryLimitMB     int           `json:"memoryLimitMB"`
	DiskReadLimitMBs  int           `json:"diskReadLimitMBs"`
	DiskWriteLimitMBs int           `json:"diskWriteLimitMBs"`
}

// Presets have stable names; changing a workload requires a new preset version.
var Presets = map[string]Profile{
	"smoke":    {"smoke", 2, 30 * time.Second, 75 * time.Second, 2, 1024, 32, 16},
	"standard": {"standard", 4, 30 * time.Second, 30 * time.Minute, 2, 2048, 16, 8},
	"hostile":  {"hostile", 8, 30 * time.Second, 60 * time.Minute, 1, 1024, 8, 4},
}

const drainWindow = 60 * time.Second // fixture's per-stage timeout
const pollInterval = time.Second
const replacementWindow = 10 * time.Second // maximum continuous time below target concurrency
const fixtureFailure = "soak_fixture_failure"
const fixtureFailureReason = fixtureFailure + ": " + fixtureFailure

type invalidReason string

const (
	containerLaunchFailed invalidReason = "container-launch-failed"
	daemonHealthFailed    invalidReason = "daemon-health-check-failed"
	loadInjectorCrashed   invalidReason = "load-injector-crashed"
	hostOOMKilled         invalidReason = "host-oom-killed" // supplied by the #1480 supervisor
	observationLost       invalidReason = "observation-path-lost"
)

const (
	admitted        = "soak: admitted"
	heldRamp        = "soak: held (ramp)"
	heldCapacity    = "soak: held (instance max-parallel)"
	refusedDeadline = "soak: refused (ramp deadline exceeded)"
)

type decision struct {
	At           time.Time `json:"at"`
	Code         string    `json:"code"`
	AcceptanceID string    `json:"acceptanceID,omitempty"`
	RunID        string    `json:"runID,omitempty"`
}

type signals struct {
	Throughput           *bool `json:"throughput"`
	NoInfraEscalations   *bool `json:"noInfraEscalations"`
	NoWedgedRuns         *bool `json:"noWedgedRuns"`
	SustainedConcurrency *bool `json:"sustainedConcurrency"`
}

type result struct {
	Profile            Profile        `json:"profile"`
	Verdict            string         `json:"verdict"`
	InvalidReason      invalidReason  `json:"invalidReason,omitempty"`
	Error              string         `json:"error,omitempty"`
	Started            time.Time      `json:"started"`
	SustainStarted     time.Time      `json:"sustainStarted"`
	SustainEnded       time.Time      `json:"sustainEnded"`
	Signals            signals        `json:"signals"`
	Admissions         []decision     `json:"admissions"`
	Completed          *int           `json:"completed"`
	ExpectedFailures   *int           `json:"expectedFailures"`
	UnexpectedFailures []string       `json:"unexpectedFailures,omitempty"`
	Wedged             []string       `json:"wedged,omitempty"`
	RampRefused        bool           `json:"rampRefused"`
	LongestUnderfill   *time.Duration `json:"longestUnderfill"`
}

func (r *result) invalidate(reason invalidReason, err error) {
	r.Verdict, r.InvalidReason = "invalid", reason
	r.Signals = signals{} // unavailable is not a measured zero
	r.Completed, r.ExpectedFailures = nil, nil
	r.LongestUnderfill = nil
	if err != nil {
		r.Error = err.Error()
	}
}
