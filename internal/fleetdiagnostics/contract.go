// Package fleetdiagnostics provides a bounded reference consumer for opted-in
// fleet observations. Tenant authentication and inventory enrollment belong to
// the receiving company; an organization label never grants access.
package fleetdiagnostics

import "time"

const (
	// HeartbeatEvent identifies the version-one fleet observation contract.
	HeartbeatEvent = "goobers.fleet.heartbeat"
	// FeatureEvent identifies absolute, per-boot usage-window observations.
	FeatureEvent = "goobers.feature.usage"
	// SchemaVersion is the only currently supported record schema.
	SchemaVersion = 1
)

// Identity is metadata, not authentication. The receiver supplies the tenant.
// Empty GaggleID denotes the deployment-level observation.
type Identity struct {
	Organization string `json:"organization,omitempty"`
	Environment  string `json:"environment,omitempty"`
	DeploymentID string `json:"deploymentId"`
	InstanceID   string `json:"instanceId"`
	GaggleID     string `json:"gaggleId,omitempty"`
	OwnerRef     string `json:"ownerRef,omitempty"`
	Component    string `json:"component"`
}

// Window identifies a monotonically sequenced observation within one process.
// Counts are absolute snapshots; retransmission must never add to a counter.
type Window struct {
	BootID        string    `json:"bootId"`
	BootStartedAt time.Time `json:"bootStartedAt"`
	Sequence      int64     `json:"sequence"`
	ObservedAt    time.Time `json:"observedAt"`
	WindowStart   time.Time `json:"windowStart"`
	Coverage      string    `json:"windowCoverage"`
}

// Heartbeat reports observations, not an inferred root cause. Nil counters and
// timestamps mean unknown. Idle is distinct from no progress with eligible work.
type Heartbeat struct {
	RequiredMCP *MCPHealth `json:"requiredMcp,omitempty"`
	Identity
	Window
	State                string     `json:"state"`
	ReasonCode           string     `json:"reasonCode"`
	LastUsefulProgressAt *time.Time `json:"lastUsefulProgressAt,omitempty"`
	OldestEligibleAt     *time.Time `json:"oldestEligibleAt,omitempty"`
	EligibleCount        *int64     `json:"eligibleCount,omitempty"`
	InflightCount        *int64     `json:"inflightCount,omitempty"`
	AdmissionLimit       *int64     `json:"admissionLimit,omitempty"`
	MissingWorkerCount   *int64     `json:"missingWorkerCount,omitempty"`
	RetryCount           *int64     `json:"retryCount,omitempty"`
	NoWorkCount          *int64     `json:"noWorkCount,omitempty"`
	Version              string     `json:"version,omitempty"`
	BuildCommit          string     `json:"buildCommit,omitempty"`
	Channel              string     `json:"channel,omitempty"`
	Platform             string     `json:"platform,omitempty"`
}

// FeatureUsage reports one documented feature's absolute count in a window.
// Count may be omitted; zero is meaningful only with complete coverage.
// Windows belong to a boot. A new boot resets the displayed observation and
// never adds its count to the previous boot's window.
type FeatureUsage struct {
	Identity
	Window
	WindowEnd  time.Time `json:"windowEnd"`
	FeatureID  string    `json:"featureId"`
	Configured bool      `json:"configured"`
	Count      *int64    `json:"count,omitempty"`
}

// FeatureIDs is the bounded catalogue understood by this contract. Capabilities
// must be added deliberately; arbitrary capability.* names are not accepted.
func FeatureIDs() []string {
	return []string{"runner.local", "runner.engine", "adapter.copilot", "adapter.claude", "adapter.codex", "provider.github", "provider.gitea", "provider.azure-devops", "dsl.v1", "dsl.v2", "dsl.v3"}
}

// MCPHealth is an independent unresolved-tool condition, not the gaggle's work
// state. Partial history cannot establish that every context has recovered.
type MCPHealth struct {
	State       string     `json:"state"`
	Coverage    string     `json:"coverage"`
	Reason      string     `json:"reason,omitempty"`
	ActiveCount *int64     `json:"activeCount,omitempty"`
	ObservedAt  *time.Time `json:"observedAt,omitempty"`
	Workflow    string     `json:"workflow,omitempty"`
	Stage       string     `json:"stage,omitempty"`
	Adapter     string     `json:"adapter,omitempty"`
	Branch      int        `json:"branch,omitempty"`
}
