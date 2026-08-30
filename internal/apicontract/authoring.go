package apicontract

import (
	"fmt"
	"net/http"
	"slices"
)

// AuthoringSchemaVersion identifies the configuration-authoring wire schema.
const AuthoringSchemaVersion = "v1alpha1"

// Config authoring routes are versioned contract declarations for the
// separately implemented authoring API. They are not registered by the daemon
// until their backing services are configured.
const (
	ConfigSourcesPath         = V1Prefix + "/config/sources"
	ConfigSourceDocumentsPath = V1Prefix + "/config/sources/{source}/documents"
	ConfigSourceDocumentPath  = V1Prefix + "/config/sources/{source}/document"
	ConfigSourcePreviewPath   = V1Prefix + "/config/sources/{source}/preview"
	ConfigSourceChangesPath   = V1Prefix + "/config/sources/{source}/changes"
)

// Stable configuration-authoring route IDs.
const (
	RouteConfigSources         RouteID = "configSources"
	RouteConfigSourceDocuments RouteID = "configSourceDocuments"
	RouteConfigSourceDocument  RouteID = "configSourceDocument"
	RouteConfigSourcePreview   RouteID = "configSourcePreview"
	RouteConfigSourceChanges   RouteID = "configSourceChanges"
)

var v1ConfigAuthoringRoutes = []Route{
	{ID: RouteConfigSources, Method: http.MethodGet, Path: ConfigSourcesPath, ActionClass: ActionReadOnlyNavigation, Cost: CostBounded, Budget: BoundedBudget},
	{ID: RouteConfigSourceDocuments, Method: http.MethodGet, Path: ConfigSourceDocumentsPath, ActionClass: ActionReadOnlyNavigation, Cost: CostBounded, Budget: BoundedBudget},
	{ID: RouteConfigSourceDocument, Method: http.MethodGet, Path: ConfigSourceDocumentPath, ActionClass: ActionReadOnlyNavigation, Cost: CostBounded, Budget: BoundedBudget},
	{ID: RouteConfigSourcePreview, Method: http.MethodPost, Path: ConfigSourcePreviewPath, ActionClass: ActionConfigTime, Cost: CostMutation, Budget: MutationBudget},
	{ID: RouteConfigSourceChanges, Method: http.MethodPut, Path: ConfigSourceChangesPath, ActionClass: ActionConfigTime, Cost: CostMutation, Budget: MutationBudget},
}

// V1ConfigAuthoringRoutes returns the configuration-authoring route contract.
func V1ConfigAuthoringRoutes() []Route {
	return slices.Clone(v1ConfigAuthoringRoutes)
}

// ConfigSourceKind describes how a source is governed without exposing its
// resolved checkout or host filesystem location.
type ConfigSourceKind string

// Known configuration source kinds.
const (
	ConfigSourceLocal    ConfigSourceKind = "local"
	ConfigSourceGit      ConfigSourceKind = "git"
	ConfigSourceProvider ConfigSourceKind = "provider"
)

// ConfigSourceCapabilities advertises operations supported by one source.
type ConfigSourceCapabilities struct {
	Read        bool `json:"read"`
	Validate    bool `json:"validate"`
	DirectWrite bool `json:"directWrite"`
	ReviewWrite bool `json:"reviewWrite"`
}

// ConfigSourceDescriptor is the browser-safe identity and current revision of
// a configuration source.
type ConfigSourceDescriptor struct {
	ID           string                   `json:"id"`
	DisplayName  string                   `json:"displayName"`
	Kind         ConfigSourceKind         `json:"kind"`
	Revision     string                   `json:"revision"`
	Capabilities ConfigSourceCapabilities `json:"capabilities"`
}

// ConfigSourcePage lists configuration sources available to the principal.
type ConfigSourcePage struct {
	APIVersion    string                   `json:"apiVersion"`
	SchemaVersion string                   `json:"schemaVersion"`
	Items         []ConfigSourceDescriptor `json:"items"`
}

// ConfigDocumentKind identifies a known authored definition.
type ConfigDocumentKind string

// Known authored definition kinds.
const (
	ConfigDocumentManifest ConfigDocumentKind = "manifest"
	ConfigDocumentInstance ConfigDocumentKind = "instance"
	ConfigDocumentGaggle   ConfigDocumentKind = "gaggle"
	ConfigDocumentWorkflow ConfigDocumentKind = "workflow"
	ConfigDocumentGoober   ConfigDocumentKind = "goober"
	ConfigDocumentSupport  ConfigDocumentKind = "support"
)

// ConfigDefinitionReference links a source document to an authored definition.
type ConfigDefinitionReference struct {
	Kind   ConfigDocumentKind `json:"kind"`
	Name   string             `json:"name"`
	Gaggle string             `json:"gaggle,omitempty"`
}

// ConfigDocumentDescriptor identifies a logical source document. Path is
// relative to the source and is never a resolved host path.
type ConfigDocumentDescriptor struct {
	Path       string                     `json:"path"`
	MediaType  string                     `json:"mediaType"`
	ETag       string                     `json:"etag"`
	Editable   bool                       `json:"editable"`
	Definition *ConfigDefinitionReference `json:"definition,omitempty"`
}

// ConfigDocumentPage lists documents at one source revision.
type ConfigDocumentPage struct {
	APIVersion    string                     `json:"apiVersion"`
	SchemaVersion string                     `json:"schemaVersion"`
	SourceID      string                     `json:"sourceId"`
	Revision      string                     `json:"revision"`
	Items         []ConfigDocumentDescriptor `json:"items"`
}

// ConfigDocumentRequest selects one logical document without putting an
// arbitrarily nested path into an HTTP route segment.
type ConfigDocumentRequest struct {
	Path string `json:"path"`
}

// ConfigDocument contains one source document and its concurrency token.
type ConfigDocument struct {
	APIVersion    string                   `json:"apiVersion"`
	SchemaVersion string                   `json:"schemaVersion"`
	SourceID      string                   `json:"sourceId"`
	Revision      string                   `json:"revision"`
	Document      ConfigDocumentDescriptor `json:"document"`
	Content       string                   `json:"content"`
}

// ConfigChangeOperation is one candidate document operation.
type ConfigChangeOperation string

// Known configuration change operations.
const (
	ConfigChangeUpsert ConfigChangeOperation = "upsert"
	ConfigChangeDelete ConfigChangeOperation = "delete"
)

// ConfigDocumentChange is one logical document change. BaseETag is required for
// an existing document and empty only when creating a new document.
type ConfigDocumentChange struct {
	Path      string                `json:"path"`
	Operation ConfigChangeOperation `json:"operation"`
	BaseETag  string                `json:"baseEtag,omitempty"`
	Content   *string               `json:"content,omitempty"`
}

// Validate rejects operation shapes that cannot be represented by the
// TypeScript discriminated union.
func (c ConfigDocumentChange) Validate() error {
	switch c.Operation {
	case ConfigChangeUpsert:
		if c.Content == nil {
			return fmt.Errorf("upsert %q requires content", c.Path)
		}
	case ConfigChangeDelete:
		if c.Content != nil {
			return fmt.Errorf("delete %q cannot include content", c.Path)
		}
		if c.BaseETag == "" {
			return fmt.Errorf("delete %q requires baseEtag", c.Path)
		}
	default:
		return fmt.Errorf("change %q has unsupported operation %q", c.Path, c.Operation)
	}
	return nil
}

// ConfigChangeSet groups changes that must validate and persist together.
type ConfigChangeSet struct {
	BaseRevision string                 `json:"baseRevision"`
	Changes      []ConfigDocumentChange `json:"changes"`
}

// ConfigDiagnosticSeverity is the validation diagnostic severity.
type ConfigDiagnosticSeverity string

// Known configuration diagnostic severities.
const (
	ConfigDiagnosticError   ConfigDiagnosticSeverity = "error"
	ConfigDiagnosticWarning ConfigDiagnosticSeverity = "warning"
)

// ConfigDiagnosticLocation maps a diagnostic to a logical source location.
type ConfigDiagnosticLocation struct {
	Path      string `json:"path"`
	Line      int    `json:"line,omitempty"`
	Column    int    `json:"column,omitempty"`
	EndLine   int    `json:"endLine,omitempty"`
	EndColumn int    `json:"endColumn,omitempty"`
}

// ConfigDiagnostic is a safe, structured authoring diagnostic.
type ConfigDiagnostic struct {
	Code     string                    `json:"code"`
	Severity ConfigDiagnosticSeverity  `json:"severity"`
	Message  string                    `json:"message"`
	Scope    string                    `json:"scope,omitempty"`
	Location *ConfigDiagnosticLocation `json:"location,omitempty"`
}

// ConfigDiff is a bounded normalized unified diff.
type ConfigDiff struct {
	Format    string `json:"format"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
}

// ConfigChangePreviewRequest asks the server to validate a candidate snapshot.
type ConfigChangePreviewRequest struct {
	ChangeSet ConfigChangeSet `json:"changeSet"`
}

// ConfigChangePreview is the side-effect-free validation and diff result.
type ConfigChangePreview struct {
	APIVersion    string             `json:"apiVersion"`
	SchemaVersion string             `json:"schemaVersion"`
	SourceID      string             `json:"sourceId"`
	BaseRevision  string             `json:"baseRevision"`
	PreviewID     string             `json:"previewId"`
	Eligible      bool               `json:"eligible"`
	Diagnostics   []ConfigDiagnostic `json:"diagnostics"`
	Diff          ConfigDiff         `json:"diff"`
}

// ConfigWriteStrategy selects a capability advertised by the source.
type ConfigWriteStrategy string

// Known configuration write strategies.
const (
	ConfigWriteDirect ConfigWriteStrategy = "direct"
	ConfigWriteReview ConfigWriteStrategy = "review"
)

// ConfigWriteRequest applies the exact candidate identified by PreviewID.
type ConfigWriteRequest struct {
	PreviewID string              `json:"previewId"`
	ChangeSet ConfigChangeSet     `json:"changeSet"`
	Strategy  ConfigWriteStrategy `json:"strategy"`
	Summary   string              `json:"summary,omitempty"`
}

// ConfigReviewReference describes a provider-neutral review result.
type ConfigReviewReference struct {
	ID     string `json:"id"`
	URL    string `json:"url"`
	Branch string `json:"branch,omitempty"`
	Commit string `json:"commit,omitempty"`
}

// ConfigWriteOutcome reports the durable result of a configuration write.
type ConfigWriteOutcome struct {
	APIVersion       string                 `json:"apiVersion"`
	SchemaVersion    string                 `json:"schemaVersion"`
	SourceID         string                 `json:"sourceId"`
	BaseRevision     string                 `json:"baseRevision"`
	Revision         string                 `json:"revision,omitempty"`
	Strategy         ConfigWriteStrategy    `json:"strategy"`
	ChangedDocuments []string               `json:"changedDocuments"`
	Review           *ConfigReviewReference `json:"review,omitempty"`
	SourceApplied    string                 `json:"sourceApplied,omitempty"`
}

// ConfigAuthoringErrorCode is a stable machine-readable authoring failure.
type ConfigAuthoringErrorCode string

// Stable authoring error codes.
const (
	CodeConfigSourceNotFound        ConfigAuthoringErrorCode = "config_source_not_found"
	CodeConfigDocumentNotFound      ConfigAuthoringErrorCode = "config_document_not_found"
	CodeConfigStaleRevision         ConfigAuthoringErrorCode = "config_stale_revision"
	CodeConfigUnsupportedCapability ConfigAuthoringErrorCode = "config_unsupported_capability"
	CodeConfigValidationFailed      ConfigAuthoringErrorCode = "config_validation_failed"
	CodeConfigAuthorizationFailed   ConfigAuthoringErrorCode = "config_authorization_failed"
	CodeConfigProjectionLag         ConfigAuthoringErrorCode = "config_projection_lag"
)

var configAuthoringErrorCodes = []ConfigAuthoringErrorCode{
	CodeConfigSourceNotFound,
	CodeConfigDocumentNotFound,
	CodeConfigStaleRevision,
	CodeConfigUnsupportedCapability,
	CodeConfigValidationFailed,
	CodeConfigAuthorizationFailed,
	CodeConfigProjectionLag,
}

// ConfigAuthoringErrorCodes returns the stable authoring error code registry.
func ConfigAuthoringErrorCodes() []ConfigAuthoringErrorCode {
	return slices.Clone(configAuthoringErrorCodes)
}

// ConfigAuthoringError is safe to return to a Portal client.
type ConfigAuthoringError struct {
	Code    ConfigAuthoringErrorCode `json:"code"`
	Message string                   `json:"message"`
}

// ConfigAuthoringErrorEnvelope is the typed error response for authoring routes.
type ConfigAuthoringErrorEnvelope struct {
	Error ConfigAuthoringError `json:"error"`
}
