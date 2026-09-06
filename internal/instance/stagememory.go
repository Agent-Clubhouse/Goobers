package instance

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Per-stage memory bound policy (#4070).
//
// This is the decision — what number, applied through which mechanism — kept
// separate from internal/platform/proc, which owns the mechanism. It is pure,
// so the reasoning is testable on hosts that have no cgroups to enforce it
// with.
//
// WHY THERE IS NO DERIVED DEFAULT.
//
// The obvious default — the pod's cgroup limit less a reserve for the daemon —
// was implemented first and does not survive the incident's own numbers. On
// the production 10Gi pod it yields a 9Gi per-stage bound, which would have
// bounded the 9.8 and 9.9 GiB children and NOT the 7.4 or 5.07 GiB ones. Those
// smaller children still killed the daemon, because the cgroup simultaneously
// held ~1.2Gi of page cache, the daemon itself, and SIBLING stages: one
// implementation run plus five back-to-back pr-remediation runs were in flight
// together. A single-stage bound of (limit - reserve) only ever protects
// against one stage at a time.
//
// Dividing by a concurrency limit does not rescue it either: MaxConcurrentRuns
// is per workflow identity, not a global cap, and the incident's six
// concurrent stages spanned several identities. There is no quantity the
// daemon can read that yields a bound which is both tight enough to have
// prevented these kills and loose enough not to refuse the very same agentic
// work — a 9.8 GiB child may be a runaway or may be a large repository's
// legitimate review, and nothing in the process tells them apart.
//
// So the bound is EXPLICIT ONLY. An operator who sets it gets real
// enforcement; one who does not gets an unambiguous startup report saying
// stages are unbounded and share the daemon's cgroup. Shipping a guessed
// number would either fail to protect (and read as protection) or kill
// legitimate work — and a bound that reads as protection while enforcing
// nothing is the worse of the two.

// StageMemoryBound is the resolved per-stage memory policy.
type StageMemoryBound struct {
	// MaxBytes is the bound, or 0 when no bound is configured.
	MaxBytes uint64
	// AllowAddressSpaceFallback permits RLIMIT_AS where no child cgroup can be
	// created. It tracks MaxBytes: every bound here is one an operator chose,
	// so applying it through the address-space proxy is their decision to
	// make. Were a derived default ever added, it must NOT set this — see the
	// mechanism note in internal/platform/proc/memlimit.go for why a proxy
	// bound is only ever safe on a number somebody picked deliberately.
	AllowAddressSpaceFallback bool
	// Source explains where MaxBytes came from — or why there is none — for
	// the daemon's startup report.
	Source string
}

// Enforced reports whether any bound will be applied.
func (b StageMemoryBound) Enforced() bool { return b.MaxBytes > 0 }

// UnboundedStageMemoryWarning is the startup line for an instance running
// stages unbounded inside the daemon's own cgroup.
//
// It exists because this is a STRUCTURAL hazard that is invisible in a healthy
// config: nothing is misconfigured, no check fails, and the daemon dies weeks
// later with an exit code that the container erases on restart (memory.events
// counters reset with the cgroup, so a post-hoc look reads oom_kill 0 on a pod
// that was OOM-killed 30 minutes earlier). The only signal that can precede
// that is the daemon saying, every start, that it is unprotected.
const UnboundedStageMemoryWarning = "stage subprocesses run UNBOUNDED inside this daemon's own memory cgroup: " +
	"a single heavy stage can get the control-plane daemon OOM-killed, taking every in-flight run with it (#4070). " +
	"Set runner.stageMemoryLimit to bound one stage's memory"

// ResolveStageMemoryBound decides the per-stage bound.
func (c *RunnerConfig) ResolveStageMemoryBound() (StageMemoryBound, error) {
	if c == nil || c.StageMemoryLimit == "" {
		return StageMemoryBound{Source: "runner.stageMemoryLimit is not set"}, nil
	}
	quantity, err := resource.ParseQuantity(c.StageMemoryLimit)
	if err != nil {
		return StageMemoryBound{}, fmt.Errorf("runner.stageMemoryLimit %q is not a Kubernetes quantity (e.g. \"8Gi\"): %w",
			c.StageMemoryLimit, err)
	}
	value := quantity.Value()
	if value <= 0 {
		return StageMemoryBound{}, fmt.Errorf("runner.stageMemoryLimit %q must be positive", c.StageMemoryLimit)
	}
	return StageMemoryBound{
		MaxBytes:                  uint64(value),
		AllowAddressSpaceFallback: true,
		Source:                    "runner.stageMemoryLimit " + c.StageMemoryLimit,
	}, nil
}
