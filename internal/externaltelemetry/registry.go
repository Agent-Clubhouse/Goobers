package externaltelemetry

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
)

type configuredConnector struct {
	config    ConnectorConfig
	connector Connector
	limits    QueryLimits
	shapes    []ResultShape
}

type registeredFactory struct {
	factory    Factory
	definition Definition
}

// Registry keeps plugin registration separate from workflow interpretation.
type Registry struct {
	factories  map[string]registeredFactory
	connectors map[string]configuredConnector
}

// NewRegistry returns an empty connector registry.
func NewRegistry() *Registry {
	return &Registry{
		factories:  make(map[string]registeredFactory),
		connectors: make(map[string]configuredConnector),
	}
}

// Register adds one connector factory by its declared kind and version.
func (r *Registry) Register(factory Factory) error {
	if factory == nil {
		return fmt.Errorf("external telemetry: cannot register a nil factory")
	}
	definition := factory.Definition()
	if definition.Kind == "" || definition.Version == "" {
		return fmt.Errorf("external telemetry: connector kind and version are required")
	}
	if !json.Valid(definition.ConfigurationSchema) {
		return fmt.Errorf("external telemetry: %s@%s has an invalid configuration schema", definition.Kind, definition.Version)
	}
	if definition.QueryLanguage == "" || len(definition.Shapes) == 0 {
		return fmt.Errorf("external telemetry: %s@%s must declare a query language and result shapes", definition.Kind, definition.Version)
	}
	seenShapes := make(map[ResultShape]struct{}, len(definition.Shapes))
	for _, shape := range definition.Shapes {
		if !validResultShape(shape) {
			return fmt.Errorf("external telemetry: %s@%s declares invalid result shape %q", definition.Kind, definition.Version, shape)
		}
		if _, exists := seenShapes[shape]; exists {
			return fmt.Errorf("external telemetry: %s@%s declares duplicate result shape %q", definition.Kind, definition.Version, shape)
		}
		seenShapes[shape] = struct{}{}
	}
	key := factoryKey(definition.Kind, definition.Version)
	if _, exists := r.factories[key]; exists {
		return fmt.Errorf("external telemetry: connector factory %q is already registered", key)
	}
	definition.ConfigurationSchema = append(json.RawMessage(nil), definition.ConfigurationSchema...)
	definition.AuthenticationModes = append([]string(nil), definition.AuthenticationModes...)
	definition.Shapes = append([]ResultShape(nil), definition.Shapes...)
	r.factories[key] = registeredFactory{factory: factory, definition: definition}
	return nil
}

// Configure validates and constructs one named connector before any run starts.
func (r *Registry) Configure(config ConnectorConfig, baseClient *http.Client, registrar SecretRegistrar) error {
	if err := config.Validate(); err != nil {
		return fmt.Errorf("external telemetry connector %q: %w", config.Name, err)
	}
	if _, exists := r.connectors[config.Name]; exists {
		return fmt.Errorf("external telemetry connector %q is already configured", config.Name)
	}
	registered, exists := r.factories[factoryKey(config.Kind, config.Version)]
	if !exists {
		return fmt.Errorf("external telemetry connector %q: no plugin registered for %s@%s", config.Name, config.Kind, config.Version)
	}
	factory := registered.factory
	definition := registered.definition
	mode := config.Auth.Mode
	if mode == "" {
		mode = AuthNone
	}
	if !slices.Contains(definition.AuthenticationModes, mode) {
		return fmt.Errorf("external telemetry connector %q: auth mode %q is not supported by %s@%s", config.Name, mode, config.Kind, config.Version)
	}
	if err := factory.ValidateConfig(config.Config); err != nil {
		return fmt.Errorf("external telemetry connector %q config: %w", config.Name, err)
	}
	limits, err := config.Policy.Limits()
	if err != nil {
		return fmt.Errorf("external telemetry connector %q policy: %w", config.Name, err)
	}
	connector, err := factory.Build(config, BuildOptions{
		HTTPClient: policyClient(baseClient, config.Network),
		Registrar:  registrar,
	})
	if err != nil {
		return fmt.Errorf("external telemetry connector %q: %w", config.Name, err)
	}
	if connector == nil {
		return fmt.Errorf("external telemetry connector %q: plugin returned a nil connector", config.Name)
	}
	descriptor := connector.Descriptor()
	if descriptor.Kind != config.Kind || descriptor.Version != config.Version || descriptor.SourceID == "" {
		return fmt.Errorf("external telemetry connector %q: plugin returned mismatched or incomplete identity", config.Name)
	}
	r.connectors[config.Name] = configuredConnector{
		config:    config,
		connector: connector,
		limits:    limits,
		shapes:    append([]ResultShape(nil), definition.Shapes...),
	}
	return nil
}

// Names returns configured connector names in deterministic order.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.connectors))
	for name := range r.connectors {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (r *Registry) connector(name string) (configuredConnector, error) {
	entry, exists := r.connectors[name]
	if !exists {
		return configuredConnector{}, NewQueryError(
			"connector_not_found",
			"configuration",
			false,
			fmt.Errorf("external telemetry connector %q is not configured", name),
		)
	}
	return entry, nil
}

func factoryKey(kind, version string) string {
	return kind + "@" + version
}
