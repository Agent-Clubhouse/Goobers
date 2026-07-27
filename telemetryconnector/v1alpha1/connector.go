// Package v1alpha1 is the versioned extension API for external operational
// telemetry connectors. Extension packages register a Factory from init and are
// linked into a custom Goobers distribution with a blank import.
package v1alpha1

import (
	"fmt"
	"sort"
	"sync"

	"github.com/goobers/goobers/internal/externaltelemetry"
)

const (
	// ConnectorContractVersion is this extension API's compatibility version.
	ConnectorContractVersion = externaltelemetry.ConnectorContractVersion
	// ResultSchemaVersion identifies normalized query artifacts.
	ResultSchemaVersion = externaltelemetry.ResultSchemaVersion
	// MinimumMaxBytes is the smallest supported normalized artifact limit.
	MinimumMaxBytes = externaltelemetry.MinimumMaxBytes
)

type (
	// DataState is the normalized present/empty/stale/failed vocabulary.
	DataState = externaltelemetry.DataState
	// ResultShape selects point, table, or time-series interpretation.
	ResultShape = externaltelemetry.ResultShape
	// ColumnType is the normalized cross-provider value vocabulary.
	ColumnType = externaltelemetry.ColumnType
	// Column is one typed result column.
	Column = externaltelemetry.Column
	// Window is the requested query time range.
	Window = externaltelemetry.Window
	// QueryLimits contains effective host-enforced query bounds.
	QueryLimits = externaltelemetry.QueryLimits
	// QueryRequest is the provider-neutral connector request.
	QueryRequest = externaltelemetry.QueryRequest
	// SourceResult is the typed adapter response before host bounds.
	SourceResult = externaltelemetry.SourceResult
	// ConnectorIdentity pins the selected named connector and plugin version.
	ConnectorIdentity = externaltelemetry.ConnectorIdentity
	// SourceIdentity identifies a remote source without credentials.
	SourceIdentity = externaltelemetry.SourceIdentity
	// QueryProvenance retains non-secret query identity.
	QueryProvenance = externaltelemetry.QueryProvenance
	// ResultMetadata records retries, bounds, and truncation.
	ResultMetadata = externaltelemetry.ResultMetadata
	// FailureInfo is the durable non-secret failure classification.
	FailureInfo = externaltelemetry.FailureInfo
	// ResultArtifact is the versioned normalized query evidence.
	ResultArtifact = externaltelemetry.ResultArtifact
	// Descriptor identifies a configured connector and source.
	Descriptor = externaltelemetry.Descriptor
	// Connector executes read-only queries.
	Connector = externaltelemetry.Connector
	// Definition declares a factory's schema and capabilities.
	Definition = externaltelemetry.Definition
	// Factory validates and constructs one connector kind and version.
	Factory = externaltelemetry.Factory
	// HTTPDoer is the host-owned network-policy HTTP boundary.
	HTTPDoer = externaltelemetry.HTTPDoer
	// SecretRegistrar registers resolved credentials for journal redaction.
	SecretRegistrar = externaltelemetry.SecretRegistrar
	// BuildOptions are host-owned services supplied to factories.
	BuildOptions = externaltelemetry.BuildOptions
	// CredentialRef references env/file credential material.
	CredentialRef = externaltelemetry.CredentialRef
	// AuthConfig selects credential acquisition without embedding values.
	AuthConfig = externaltelemetry.AuthConfig
	// PolicyConfig is the host-side maximum policy.
	PolicyConfig = externaltelemetry.PolicyConfig
	// NetworkPolicy is the connector network allowlist.
	NetworkPolicy = externaltelemetry.NetworkPolicy
	// ConnectorConfig configures one named connector.
	ConnectorConfig = externaltelemetry.ConnectorConfig
	// QueryError classifies connector failures for retry and audit semantics.
	QueryError = externaltelemetry.QueryError
)

const (
	// DataPresent and the remaining Data* values are normalized query states.
	DataPresent = externaltelemetry.DataPresent
	// DataEmpty is a valid zero-row result.
	DataEmpty = externaltelemetry.DataEmpty
	// DataStale has present rows but an absent or old watermark.
	DataStale = externaltelemetry.DataStale
	// DataFailed records an explicit query failure.
	DataFailed = externaltelemetry.DataFailed

	// ShapePoint and the remaining Shape* values are supported result shapes.
	ShapePoint = externaltelemetry.ShapePoint
	// ShapeTable represents typed tabular rows.
	ShapeTable = externaltelemetry.ShapeTable
	// ShapeTimeSeries represents rows containing a datetime column.
	ShapeTimeSeries = externaltelemetry.ShapeTimeSeries

	// TypeString and the remaining Type* values are normalized column types.
	TypeString = externaltelemetry.TypeString
	// TypeInteger is a signed integral value.
	TypeInteger = externaltelemetry.TypeInteger
	// TypeNumber is an integral or floating-point value.
	TypeNumber = externaltelemetry.TypeNumber
	// TypeBoolean is a boolean value.
	TypeBoolean = externaltelemetry.TypeBoolean
	// TypeDateTime is an RFC 3339-compatible timestamp.
	TypeDateTime = externaltelemetry.TypeDateTime
	// TypeDuration is a source duration string.
	TypeDuration = externaltelemetry.TypeDuration
	// TypeJSON is a provider-neutral JSON value.
	TypeJSON = externaltelemetry.TypeJSON

	// AuthNone and the remaining Auth* values identify credential acquisition modes.
	AuthNone = externaltelemetry.AuthNone
	// AuthBearerToken resolves an env/file bearer token.
	AuthBearerToken = externaltelemetry.AuthBearerToken
	// AuthAzureCLI uses the current Azure CLI login.
	AuthAzureCLI = externaltelemetry.AuthAzureCLI
	// AuthWorkloadIdentity uses Azure workload identity.
	AuthWorkloadIdentity = externaltelemetry.AuthWorkloadIdentity
	// AuthManagedIdentity uses an Azure managed identity.
	AuthManagedIdentity = externaltelemetry.AuthManagedIdentity
	// AuthDefaultAzure uses the Azure SDK default credential chain.
	AuthDefaultAzure = externaltelemetry.AuthDefaultAzure
)

var extensionFactories = struct {
	sync.RWMutex
	byKey map[string]Factory
}{byKey: make(map[string]Factory)}

// RegisterFactory registers one compiled extension factory. Duplicate
// kind/version pairs fail rather than silently replacing an implementation.
func RegisterFactory(factory Factory) error {
	registry := externaltelemetry.NewRegistry()
	if err := registry.Register(factory); err != nil {
		return err
	}
	definition := factory.Definition()
	key := definition.Kind + "@" + definition.Version
	extensionFactories.Lock()
	defer extensionFactories.Unlock()
	if _, exists := extensionFactories.byKey[key]; exists {
		return fmt.Errorf("telemetry connector extension %q is already registered", key)
	}
	extensionFactories.byKey[key] = factory
	return nil
}

// MustRegisterFactory registers factory or panics during extension initialization.
func MustRegisterFactory(factory Factory) {
	if err := RegisterFactory(factory); err != nil {
		panic(err)
	}
}

// RegisteredFactories returns a deterministic snapshot of compiled extensions.
func RegisteredFactories() []Factory {
	extensionFactories.RLock()
	defer extensionFactories.RUnlock()
	keys := make([]string, 0, len(extensionFactories.byKey))
	for key := range extensionFactories.byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	factories := make([]Factory, 0, len(keys))
	for _, key := range keys {
		factories = append(factories, extensionFactories.byKey[key])
	}
	return factories
}

// NewQueryError returns a classified connector failure. Transport throttling
// and service errors should be retryable; auth, query, and schema errors should
// not be.
func NewQueryError(code, kind string, retryable bool, err error) error {
	return externaltelemetry.NewQueryError(code, kind, retryable, err)
}
