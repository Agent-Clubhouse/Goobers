// Package externaltelemetry defines the provider-neutral contract for querying
// operational telemetry owned by a target project.
package externaltelemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	// ConnectorContractVersion is the connector plugin API implemented here.
	ConnectorContractVersion = "v1alpha1"
	// ResultSchemaVersion identifies the normalized query-result artifact.
	ResultSchemaVersion = "goobers.dev/external-telemetry-query-result/v1alpha1"

	// DefaultTimeout and the remaining Default* values are host policy defaults.
	DefaultTimeout = 30 * time.Second
	// DefaultMaxAttempts bounds transient retries.
	DefaultMaxAttempts = 3
	// DefaultRetryBackoff is the fixed delay between retries.
	DefaultRetryBackoff = time.Second
	// DefaultMaxRows bounds normalized rows.
	DefaultMaxRows = 1000
	// DefaultMaxBytes bounds the normalized artifact.
	DefaultMaxBytes = 1 << 20
)

const (
	// AuthNone and the remaining Auth* values identify credential acquisition modes.
	AuthNone = "none"
	// AuthBearerToken resolves an env/file bearer token.
	AuthBearerToken = "bearer-token"
	// AuthAzureCLI uses the current Azure CLI login.
	AuthAzureCLI = "azure-cli"
	// AuthWorkloadIdentity uses Azure workload identity.
	AuthWorkloadIdentity = "workload-identity"
	// AuthManagedIdentity uses an Azure managed identity.
	AuthManagedIdentity = "managed-identity"
	// AuthDefaultAzure uses the Azure SDK default credential chain.
	AuthDefaultAzure = "default-azure"
)

// DataState is the interpretation-safe state of one normalized query.
type DataState string

const (
	// DataPresent and the remaining Data* values are normalized query states.
	DataPresent DataState = "present"
	// DataEmpty is a valid zero-row result.
	DataEmpty DataState = "empty"
	// DataStale has present rows but an absent or old watermark.
	DataStale DataState = "stale"
	// DataFailed records an explicit query failure.
	DataFailed DataState = "failed"
)

// ResultShape describes how consumers should interpret the typed rows.
type ResultShape string

const (
	// ShapePoint and the remaining Shape* values are supported result shapes.
	ShapePoint ResultShape = "point"
	// ShapeTable represents typed tabular rows.
	ShapeTable ResultShape = "table"
	// ShapeTimeSeries represents rows containing a datetime column.
	ShapeTimeSeries ResultShape = "time-series"
)

// ColumnType is the normalized cross-provider value vocabulary.
type ColumnType string

const (
	// TypeString and the remaining Type* values are normalized column types.
	TypeString ColumnType = "string"
	// TypeInteger is a signed integral value.
	TypeInteger ColumnType = "integer"
	// TypeNumber is an integral or floating-point value.
	TypeNumber ColumnType = "number"
	// TypeBoolean is a boolean value.
	TypeBoolean ColumnType = "boolean"
	// TypeDateTime is an RFC 3339-compatible timestamp.
	TypeDateTime ColumnType = "datetime"
	// TypeDuration is a source duration string.
	TypeDuration ColumnType = "duration"
	// TypeJSON is a provider-neutral JSON value.
	TypeJSON ColumnType = "json"
)

// Column is one typed result column. Unit is required only when the source or
// connector configuration supplies one.
type Column struct {
	Name string     `json:"name"`
	Type ColumnType `json:"type"`
	Unit string     `json:"unit,omitempty"`
}

// Window is the time range requested by the workflow.
type Window struct {
	Start *time.Time `json:"start,omitempty"`
	End   *time.Time `json:"end,omitempty"`
}

// QueryLimits lets a workflow tighten configured time, attempt, row, and byte
// limits. RetryBackoff may only be lengthened to avoid increasing source load.
type QueryLimits struct {
	Timeout      time.Duration `json:"-"`
	MaxAttempts  int           `json:"-"`
	RetryBackoff time.Duration `json:"-"`
	MaxRows      int           `json:"-"`
	MaxBytes     int           `json:"-"`
}

// QueryRequest is the provider-neutral request passed to a configured connector.
type QueryRequest struct {
	Query           string
	QueryRef        string
	Parameters      map[string]any
	Window          Window
	ExpectedColumns []Column
	Shape           ResultShape
	Freshness       time.Duration
	Limits          QueryLimits
}

// SourceResult is the typed, unbounded adapter response before host policy is
// applied. Adapters must not place credentials or request headers in it.
type SourceResult struct {
	Columns   []Column          `json:"columns"`
	Rows      [][]any           `json:"rows"`
	DataAsOf  *time.Time        `json:"dataAsOf,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	Sampled   bool              `json:"sampled,omitempty"`
	Truncated bool              `json:"truncated,omitempty"`
}

// ConnectorIdentity pins the selected named connector and plugin version.
type ConnectorIdentity struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Version string `json:"version"`
}

// SourceIdentity identifies the remote source without carrying credentials.
type SourceIdentity struct {
	ID string `json:"id"`
}

// QueryProvenance retains stable query identity without persisting request
// headers, bearer tokens, or parameter values.
type QueryProvenance struct {
	Digest          string   `json:"digest"`
	Reference       string   `json:"reference,omitempty"`
	ParameterNames  []string `json:"parameterNames,omitempty"`
	ParameterDigest string   `json:"parameterDigest,omitempty"`
}

// ResultMetadata records every host-side loss or retry applied to a result.
type ResultMetadata struct {
	Attempts    int  `json:"attempts"`
	Truncated   bool `json:"truncated,omitempty"`
	RowsDropped int  `json:"rowsDropped,omitempty"`
	Sampled     bool `json:"sampled,omitempty"`
	MaxRows     int  `json:"maxRows"`
	MaxBytes    int  `json:"maxBytes"`
}

// FailureInfo is the non-secret, machine-readable portion of a failed query.
type FailureInfo struct {
	Code      string `json:"code"`
	Kind      string `json:"kind"`
	Retryable bool   `json:"retryable,omitempty"`
}

// ResultArtifact is the versioned durable evidence emitted for every query
// attempt. Failed artifacts contain provenance and Failure but never provider
// response bodies or authentication material.
type ResultArtifact struct {
	SchemaVersion string            `json:"schemaVersion"`
	Connector     ConnectorIdentity `json:"connector"`
	Source        SourceIdentity    `json:"source"`
	Query         QueryProvenance   `json:"query"`
	Window        Window            `json:"requestedWindow"`
	QueryStarted  time.Time         `json:"queryStarted"`
	QueryEnded    time.Time         `json:"queryEnded"`
	DataAsOf      *time.Time        `json:"dataAsOf,omitempty"`
	State         DataState         `json:"state"`
	Shape         ResultShape       `json:"shape"`
	Columns       []Column          `json:"columns"`
	Rows          [][]any           `json:"rows"`
	Labels        map[string]string `json:"labels,omitempty"`
	Metadata      ResultMetadata    `json:"metadata"`
	Failure       *FailureInfo      `json:"failure,omitempty"`
}

// Descriptor is the stable identity of one configured connector.
type Descriptor struct {
	Kind     string
	Version  string
	SourceID string
}

// Connector performs read-only queries against one configured source.
type Connector interface {
	Descriptor() Descriptor
	Query(context.Context, QueryRequest) (SourceResult, error)
}

// Definition declares a plugin's stable contract and configuration schema.
type Definition struct {
	Kind                string
	Version             string
	ConfigurationSchema json.RawMessage
	AuthenticationModes []string
	QueryLanguage       string
	Shapes              []ResultShape
}

// Factory validates and constructs one connector kind. Registering another
// factory does not change workflow types or the host query path.
type Factory interface {
	Definition() Definition
	ValidateConfig(json.RawMessage) error
	Build(ConnectorConfig, BuildOptions) (Connector, error)
}

// HTTPDoer is the host-owned, network-policy-enforcing HTTP boundary supplied
// to connector factories.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// SecretRegistrar registers resolved credentials with the journal scrubber.
type SecretRegistrar interface {
	Register([]byte)
}

// BuildOptions are host-owned services available to connector factories.
type BuildOptions struct {
	HTTPClient HTTPDoer
	Registrar  SecretRegistrar
}

// CredentialRef references one env/file credential source, never an inline value.
type CredentialRef struct {
	Env  string `json:"env,omitempty" yaml:"env,omitempty"`
	File string `json:"file,omitempty" yaml:"file,omitempty"`
}

// AuthConfig selects a credential acquisition mode for a named connector.
type AuthConfig struct {
	Mode     string         `json:"mode,omitempty" yaml:"mode,omitempty"`
	Token    *CredentialRef `json:"token,omitempty" yaml:"token,omitempty"`
	Tenant   string         `json:"tenant,omitempty" yaml:"tenant,omitempty"`
	ClientID string         `json:"clientId,omitempty" yaml:"clientId,omitempty"`
}

// PolicyConfig is the host-side maximum for one named connector.
type PolicyConfig struct {
	Timeout      string `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	MaxAttempts  int    `json:"maxAttempts,omitempty" yaml:"maxAttempts,omitempty"`
	RetryBackoff string `json:"retryBackoff,omitempty" yaml:"retryBackoff,omitempty"`
	MaxRows      int    `json:"maxRows,omitempty" yaml:"maxRows,omitempty"`
	MaxBytes     int    `json:"maxBytes,omitempty" yaml:"maxBytes,omitempty"`
}

// NetworkPolicy is the only network access a host-provided connector client allows.
type NetworkPolicy struct {
	AllowedHosts []string `json:"allowedHosts,omitempty" yaml:"allowedHosts,omitempty"`
	AllowHTTP    bool     `json:"allowHTTP,omitempty" yaml:"allowHTTP,omitempty"`
}

// ConnectorConfig configures one named plugin instance in instance.yaml.
type ConnectorConfig struct {
	Name    string          `json:"name" yaml:"name"`
	Kind    string          `json:"kind" yaml:"kind"`
	Version string          `json:"version" yaml:"version"`
	Auth    AuthConfig      `json:"auth,omitempty" yaml:"auth,omitempty"`
	Policy  PolicyConfig    `json:"policy,omitempty" yaml:"policy,omitempty"`
	Network NetworkPolicy   `json:"network,omitempty" yaml:"network,omitempty"`
	Config  json.RawMessage `json:"config" yaml:"config"`
}

// Configuration is the externalTelemetry section of instance.yaml.
type Configuration struct {
	Connectors []ConnectorConfig `json:"connectors,omitempty" yaml:"connectors,omitempty"`
}

var configNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// Validate checks provider-neutral connector configuration without resolving a
// credential or performing network I/O.
func (c Configuration) Validate() error {
	seen := make(map[string]struct{}, len(c.Connectors))
	for i, connector := range c.Connectors {
		if err := connector.Validate(); err != nil {
			return fmt.Errorf("connectors[%d]: %w", i, err)
		}
		if _, exists := seen[connector.Name]; exists {
			return fmt.Errorf("connectors[%d]: duplicate name %q", i, connector.Name)
		}
		seen[connector.Name] = struct{}{}
	}
	return nil
}

// Validate checks one provider-neutral connector definition.
func (c ConnectorConfig) Validate() error {
	if !configNamePattern.MatchString(c.Name) {
		return fmt.Errorf("name %q must match %s", c.Name, configNamePattern)
	}
	if strings.TrimSpace(c.Kind) == "" || strings.TrimSpace(c.Version) == "" {
		return errors.New("kind and version are required")
	}
	if len(c.Config) == 0 || string(c.Config) == "null" {
		return errors.New("config is required")
	}
	if err := c.Auth.Validate(); err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	if _, err := c.Policy.Limits(); err != nil {
		return fmt.Errorf("policy: %w", err)
	}
	if err := c.Network.Validate(); err != nil {
		return fmt.Errorf("network: %w", err)
	}
	return nil
}

// Validate checks credential-reference shape without acquiring a credential.
func (a AuthConfig) Validate() error {
	mode := a.Mode
	if mode == "" {
		mode = AuthNone
	}
	if mode == AuthBearerToken {
		if a.Token == nil || (a.Token.Env == "") == (a.Token.File == "") {
			return errors.New("bearer-token mode requires exactly one of token.env or token.file")
		}
	} else if a.Token != nil {
		return fmt.Errorf("token is only valid for mode %q", AuthBearerToken)
	}
	if a.ClientID != "" && mode != AuthManagedIdentity {
		return fmt.Errorf("clientId is only valid for mode %q", AuthManagedIdentity)
	}
	if a.Tenant != "" && mode != AuthAzureCLI && mode != AuthDefaultAzure {
		return fmt.Errorf("tenant is only valid for modes %q and %q", AuthAzureCLI, AuthDefaultAzure)
	}
	return nil
}

// Limits parses configured policy and supplies deterministic defaults.
func (p PolicyConfig) Limits() (QueryLimits, error) {
	limits := QueryLimits{
		Timeout:      DefaultTimeout,
		MaxAttempts:  DefaultMaxAttempts,
		RetryBackoff: DefaultRetryBackoff,
		MaxRows:      DefaultMaxRows,
		MaxBytes:     DefaultMaxBytes,
	}
	var err error
	if p.Timeout != "" {
		limits.Timeout, err = positiveDuration("timeout", p.Timeout)
		if err != nil {
			return QueryLimits{}, err
		}
	}
	if p.RetryBackoff != "" {
		limits.RetryBackoff, err = positiveDuration("retryBackoff", p.RetryBackoff)
		if err != nil {
			return QueryLimits{}, err
		}
	}
	if p.MaxAttempts != 0 {
		limits.MaxAttempts = p.MaxAttempts
	}
	if p.MaxRows != 0 {
		limits.MaxRows = p.MaxRows
	}
	if p.MaxBytes != 0 {
		limits.MaxBytes = p.MaxBytes
	}
	if limits.MaxAttempts < 1 || limits.MaxAttempts > 10 {
		return QueryLimits{}, errors.New("maxAttempts must be between 1 and 10")
	}
	if limits.MaxRows < 1 {
		return QueryLimits{}, errors.New("maxRows must be positive")
	}
	if limits.MaxBytes < 256 {
		return QueryLimits{}, errors.New("maxBytes must be at least 256")
	}
	return limits, nil
}

func positiveDuration(name, value string) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("%s %q must be a positive duration", name, value)
	}
	return duration, nil
}

// Validate checks that the network allowlist contains hostnames, not URLs.
func (p NetworkPolicy) Validate() error {
	seen := make(map[string]struct{}, len(p.AllowedHosts))
	for i, host := range p.AllowedHosts {
		if strings.TrimSpace(host) != host || host == "" || strings.ContainsAny(host, "/:@") {
			return fmt.Errorf("allowedHosts[%d] %q must be a hostname without scheme, port, or path", i, host)
		}
		normalized := strings.ToLower(host)
		if _, exists := seen[normalized]; exists {
			return fmt.Errorf("allowedHosts[%d] duplicates %q", i, host)
		}
		seen[normalized] = struct{}{}
	}
	if p.AllowHTTP {
		for _, host := range p.AllowedHosts {
			if !isLoopbackHost(host) {
				return fmt.Errorf("allowHTTP is restricted to loopback hosts, got %q", host)
			}
		}
	}
	return nil
}

// Allows reports whether the policy permits one URL.
func (p NetworkPolicy) Allows(target *url.URL) bool {
	if target == nil || target.Hostname() == "" {
		return false
	}
	if target.Scheme != "https" && (target.Scheme != "http" || !p.AllowHTTP || !isLoopbackHost(target.Hostname())) {
		return false
	}
	return slices.ContainsFunc(p.AllowedHosts, func(host string) bool {
		return strings.EqualFold(host, target.Hostname())
	})
}

func isLoopbackHost(host string) bool {
	return strings.EqualFold(host, "localhost") || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
}

// QueryError classifies connector and host failures without conflating them
// with valid empty data.
type QueryError struct {
	Code      string
	Kind      string
	Retryable bool
	Err       error
}

func (e *QueryError) Error() string {
	if e.Err == nil {
		return e.Code
	}
	return e.Err.Error()
}

func (e *QueryError) Unwrap() error { return e.Err }

// NewQueryError returns a classified query failure.
func NewQueryError(code, kind string, retryable bool, err error) error {
	return &QueryError{Code: code, Kind: kind, Retryable: retryable, Err: err}
}

func classifyError(err error) *QueryError {
	var queryErr *QueryError
	if errors.As(err, &queryErr) {
		return queryErr
	}
	switch {
	case errors.Is(err, context.Canceled):
		return &QueryError{Code: "canceled", Kind: "cancellation", Err: err}
	case errors.Is(err, context.DeadlineExceeded):
		return &QueryError{Code: "timeout", Kind: "timeout", Retryable: true, Err: err}
	default:
		return &QueryError{Code: "plugin_failure", Kind: "plugin", Err: err}
	}
}
