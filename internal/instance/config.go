package instance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/internal/capability"
)

// APIVersion and Kind for instance.yaml. Mirrors the config-as-code
// apiVersion/kind convention (ARCHITECTURE.md §6) though instance.yaml is a
// provisioning file, never a CR the operator reconciles.
const (
	ConfigAPIVersion = "goobers.dev/v1alpha1"
	ConfigKind       = "Instance"
)

// Config is the parsed instance.yaml: target repo(s) + provider, token source
// refs, telemetry settings, instance-level run conditions (INST-010), and the
// timezone cron schedules evaluate in (issue #137 — previously promised by
// internal/localscheduler's own doc comments but never actually a field
// anywhere, so every schedule silently ran in whatever the host process's
// local zone happened to be).
type Config struct {
	APIVersion    string          `json:"apiVersion" yaml:"apiVersion"`
	Kind          string          `json:"kind" yaml:"kind"`
	Repos         []RepoRef       `json:"repos" yaml:"repos"`
	Telemetry     TelemetryConfig `json:"telemetry,omitempty" yaml:"telemetry,omitempty"`
	RunConditions RunConditions   `json:"runConditions,omitempty" yaml:"runConditions,omitempty"`
	// Credentials sources individual capabilities from their own token refs,
	// beyond the default of backing every credentialed capability with the
	// first repo's token (#287, multi-token credentials). Each entry points one
	// capability at a distinct token; an entry for a capability the runner would
	// otherwise default-grant to the repo token OVERRIDES that grant. This is
	// what lets an agentic stage carry a personal "Copilot Requests" PAT for the
	// model (agent:model) alongside the org-repo token for the github tool.
	Credentials []CredentialGrant `json:"credentials,omitempty" yaml:"credentials,omitempty"`
	// Timezone is an IANA location name (e.g. "America/New_York") every
	// workflow's cron schedule evaluates in. Empty defaults to UTC — a fixed,
	// reproducible default independent of the host process's own local zone,
	// which would otherwise vary by deployment and isn't itself DST-free.
	Timezone string `json:"timezone,omitempty" yaml:"timezone,omitempty"`
}

// RepoRef is a target repository this instance connects to.
type RepoRef struct {
	// Provider is the backing system. V0 supports "github" only.
	Provider string `json:"provider" yaml:"provider"`
	// Owner is the repo owner/organization.
	Owner string `json:"owner" yaml:"owner"`
	// Name is the repo name.
	Name string `json:"name" yaml:"name"`
	// Token is a reference to this repo's credential. Never an inline value
	// (CFG-009, SEC-010) — exactly one of Env or File must be set.
	Token TokenRef `json:"token" yaml:"token"`
}

// TokenRef points at a credential without storing its value: an environment
// variable name, or a path to a file containing it (SEC-*, "Env vars / token
// file" at tiers 1-2, ARCHITECTURE.md §9).
type TokenRef struct {
	// Env is the name of an environment variable holding the token.
	Env string `json:"env,omitempty" yaml:"env,omitempty"`
	// File is a path to a file whose contents are the token.
	File string `json:"file,omitempty" yaml:"file,omitempty"`
}

// CredentialGrant sources one capability from its own token ref (#287). It is
// the config surface for holding more than one token at a time and routing each
// to the capability that needs it — e.g. agent:model → a personal Copilot
// Requests PAT, separate from the repo token backing repo/issue/PR access.
type CredentialGrant struct {
	// Capability is the canonical capability string (internal/capability) this
	// token backs, e.g. "agent:model" or "repo:push" (to override the default).
	Capability string `json:"capability" yaml:"capability"`
	// Token is the source of the credential — exactly one of env or file, like
	// a repo's token; inline secret values are never permitted.
	Token TokenRef `json:"token" yaml:"token"`
}

// TelemetryConfig configures the local telemetry rollup store (§8).
type TelemetryConfig struct {
	// Enabled toggles the local SQLite rollup (OTel client construction, span
	// emission, and incremental ingest into telemetry.db). Defaults to true.
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
}

// RunConditions are instance-level run conditions (§7): max parallel runs and
// per-workflow run budgets.
type RunConditions struct {
	MaxParallelRuns int            `json:"maxParallelRuns,omitempty" yaml:"maxParallelRuns,omitempty"`
	WorkflowBudgets map[string]int `json:"workflowBudgets,omitempty" yaml:"workflowBudgets,omitempty"`
	// WorkflowDailyBudgets overrides a named workflow's runs-per-day budget
	// (#340), mirroring WorkflowBudgets' per-hour override.
	WorkflowDailyBudgets map[string]int `json:"workflowDailyBudgets,omitempty" yaml:"workflowDailyBudgets,omitempty"`
}

// TelemetryEnabled reports whether the local rollup store is enabled
// (defaults to true when unset). Wired into cmd/goobers' up.go/run.go (issue
// #129): telemetry.enabled was documented and set in the real self-hosting
// config (selfhost/instance.yaml.example) but had zero callers.
func (c *Config) TelemetryEnabled() bool {
	return c.Telemetry.Enabled == nil || *c.Telemetry.Enabled
}

// Location resolves Timezone to a *time.Location, defaulting to UTC when
// unset. Validate already rejects an unresolvable Timezone at load time, so
// this only errors if tzdata disappeared from underneath an already-loaded
// instance (e.g. between validate and use) — callers can treat a non-nil
// error here as exceptional.
func (c *Config) Location() (*time.Location, error) {
	if c.Timezone == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return nil, fmt.Errorf("load timezone %q: %w", c.Timezone, err)
	}
	return loc, nil
}

// LoadConfig reads and validates instance.yaml at path. Decoding is strict:
// unknown fields (including an inline secret value under a token ref) are
// rejected rather than silently ignored.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	jsonBytes, err := yaml.YAMLToJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	dec := json.NewDecoder(bytes.NewReader(jsonBytes))
	dec.DisallowUnknownFields()
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%s: %w (instance.yaml accepts only known fields; token refs must be "+
			"token.env or token.file — inline secret values are not permitted, CFG-009/SEC-010)", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

// Validate checks instance.yaml-level invariants: known provider, non-empty
// owner/name, exactly one token source per repo, and (if set) a resolvable
// IANA timezone — fail closed at load time rather than at the first cron
// tick that tries to use it.
func (c *Config) Validate() error {
	if c.Timezone != "" {
		if _, err := time.LoadLocation(c.Timezone); err != nil {
			return fmt.Errorf("timezone %q: %w", c.Timezone, err)
		}
	}
	for i, r := range c.Repos {
		if r.Provider != "github" {
			return fmt.Errorf("repos[%d]: unsupported provider %q (V0 supports \"github\" only)", i, r.Provider)
		}
		if r.Owner == "" || r.Name == "" {
			return fmt.Errorf("repos[%d]: owner and name are required", i)
		}
		hasEnv := r.Token.Env != ""
		hasFile := r.Token.File != ""
		if hasEnv == hasFile {
			return fmt.Errorf("repos[%d] (%s/%s): token must reference exactly one of env or file — "+
				"inline secret values are never permitted (CFG-009, SEC-010)", i, r.Owner, r.Name)
		}
	}
	seen := make(map[string]bool, len(c.Credentials))
	for i, cg := range c.Credentials {
		// Fail closed at load, not at the first stage that tries to resolve a
		// bad grant (#287): an unknown capability is a typo the compiler would
		// never see (credentials: isn't a workflow), and a token ref that names
		// neither/both of env|file can never resolve.
		if !capability.Known(cg.Capability) {
			return fmt.Errorf("credentials[%d]: unknown capability %q", i, cg.Capability)
		}
		if seen[cg.Capability] {
			return fmt.Errorf("credentials[%d]: capability %q is sourced more than once", i, cg.Capability)
		}
		seen[cg.Capability] = true
		hasEnv := cg.Token.Env != ""
		hasFile := cg.Token.File != ""
		if hasEnv == hasFile {
			return fmt.Errorf("credentials[%d] (%s): token must reference exactly one of env or file — "+
				"inline secret values are never permitted (CFG-009, SEC-010)", i, cg.Capability)
		}
	}
	return nil
}

// WriteConfig marshals cfg as YAML and writes it to path.
func WriteConfig(path string, cfg *Config) error {
	jsonBytes, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal instance config: %w", err)
	}
	yamlBytes, err := yaml.JSONToYAML(jsonBytes)
	if err != nil {
		return fmt.Errorf("marshal instance config: %w", err)
	}
	if err := os.WriteFile(path, yamlBytes, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
