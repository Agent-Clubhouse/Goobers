package instance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

// APIVersion and Kind for instance.yaml. Mirrors the config-as-code
// apiVersion/kind convention (ARCHITECTURE.md §6) though instance.yaml is a
// provisioning file, never a CR the operator reconciles.
const (
	ConfigAPIVersion = "goobers.dev/v1alpha1"
	ConfigKind       = "Instance"
)

// Config is the parsed instance.yaml: target repo(s) + provider, token source
// refs, telemetry settings, and instance-level run conditions (INST-010).
type Config struct {
	APIVersion    string          `json:"apiVersion" yaml:"apiVersion"`
	Kind          string          `json:"kind" yaml:"kind"`
	Repos         []RepoRef       `json:"repos" yaml:"repos"`
	Telemetry     TelemetryConfig `json:"telemetry,omitempty" yaml:"telemetry,omitempty"`
	RunConditions RunConditions   `json:"runConditions,omitempty" yaml:"runConditions,omitempty"`
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

// TelemetryConfig configures the local telemetry rollup store (§8).
type TelemetryConfig struct {
	// Enabled toggles the local SQLite rollup. Defaults to true.
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
}

// RunConditions are instance-level run conditions (§7): max parallel runs and
// per-workflow run budgets.
type RunConditions struct {
	MaxParallelRuns int            `json:"maxParallelRuns,omitempty" yaml:"maxParallelRuns,omitempty"`
	WorkflowBudgets map[string]int `json:"workflowBudgets,omitempty" yaml:"workflowBudgets,omitempty"`
}

// TelemetryEnabled reports whether the local rollup store is enabled
// (defaults to true when unset).
func (c *Config) TelemetryEnabled() bool {
	return c.Telemetry.Enabled == nil || *c.Telemetry.Enabled
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
// owner/name, and exactly one token source per repo.
func (c *Config) Validate() error {
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
