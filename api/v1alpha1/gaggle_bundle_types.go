package v1alpha1

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// GaggleBundleAPIVersion is the public API version for portable gaggle bundles.
	GaggleBundleAPIVersion = "goobers.dev/v1alpha1"
	// GaggleBundleKind is the kind discriminator for portable gaggle bundles.
	GaggleBundleKind = "GaggleBundle"
	// GaggleBundleSchemaVersion is the current bundle wire-schema revision.
	GaggleBundleSchemaVersion = 1
)

// GaggleBundle is a sanitized, portable snapshot of one gaggle's declarative
// definitions. Structured fields exclude credentials and runtime identity;
// companion text must pass the bundle's bounded credential and path checks.
type GaggleBundle struct {
	APIVersion    string                 `json:"apiVersion"`
	Kind          string                 `json:"kind"`
	SchemaVersion int                    `json:"schemaVersion"`
	Source        GaggleBundleSource     `json:"source"`
	ExportedAt    time.Time              `json:"exportedAt"`
	Provenance    GaggleBundleProvenance `json:"provenance"`
	Definition    GaggleBundleDefinition `json:"definition"`
	Digest        string                 `json:"digest"`
}

// GaggleBundleSource identifies the declarative source without identifying an
// instance, host, user, tenant, or credential.
type GaggleBundleSource struct {
	Name       string `json:"name"`
	APIVersion string `json:"apiVersion"`
	Digest     string `json:"digest"`
}

// GaggleBundleProvenance records how the bundle was produced. ExportedAt is
// intentionally outside the definition digest, so re-exporting unchanged
// declarative content produces the same digest.
type GaggleBundleProvenance struct {
	Exporter        string   `json:"exporter"`
	ExporterVersion string   `json:"exporterVersion"`
	ExporterCommit  string   `json:"exporterCommit,omitempty"`
	SanitizedFields []string `json:"sanitizedFields"`
}

// GaggleBundleDefinition reuses the canonical configuration API types instead
// of introducing a second gaggle/workflow/goober model.
type GaggleBundleDefinition struct {
	Gaggle       Gaggle             `json:"gaggle"`
	Workflows    []Workflow         `json:"workflows"`
	Goobers      []Goober           `json:"goobers"`
	Files        []GaggleBundleFile `json:"files,omitempty"`
	Repositories []RepoRef          `json:"repositories"`
}

type gaggleBundleGaggle struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              GaggleSpec `json:"spec"`
}

type gaggleBundleDefinitionWire struct {
	Gaggle       gaggleBundleGaggle `json:"gaggle"`
	Workflows    []Workflow         `json:"workflows"`
	Goobers      []Goober           `json:"goobers"`
	Files        []GaggleBundleFile `json:"files,omitempty"`
	Repositories []RepoRef          `json:"repositories"`
}

// MarshalJSON omits Gaggle status by construction. Status is observed runtime
// state and is never part of a portable declarative bundle.
func (d GaggleBundleDefinition) MarshalJSON() ([]byte, error) {
	return json.Marshal(gaggleBundleDefinitionWire{
		Gaggle: gaggleBundleGaggle{
			TypeMeta: d.Gaggle.TypeMeta, ObjectMeta: d.Gaggle.ObjectMeta, Spec: d.Gaggle.Spec,
		},
		Workflows: d.Workflows, Goobers: d.Goobers, Files: d.Files, Repositories: d.Repositories,
	})
}

// UnmarshalJSON restores the canonical Gaggle API type with empty runtime
// status after decoding the portable wire projection.
func (d *GaggleBundleDefinition) UnmarshalJSON(data []byte) error {
	var wire gaggleBundleDefinitionWire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("gaggle bundle definition must contain one JSON object")
		}
		return err
	}
	d.Gaggle = Gaggle{
		TypeMeta: wire.Gaggle.TypeMeta, ObjectMeta: wire.Gaggle.ObjectMeta, Spec: wire.Gaggle.Spec,
	}
	d.Workflows = wire.Workflows
	d.Goobers = wire.Goobers
	d.Files = wire.Files
	d.Repositories = wire.Repositories
	return nil
}

// GaggleBundleFile carries a referenced declarative companion file, such as a
// Goober instruction document or referenced skill package file.
type GaggleBundleFile struct {
	Path string `json:"path"`
	// ContentBase64 is bounded UTF-8 text that passed the portable companion
	// credential and host-local-path checks, encoded without modification.
	ContentBase64 string `json:"contentBase64"`
	SHA256        string `json:"sha256"`
}

// GaggleBundleImportRequest asks a destination instance to create a new gaggle
// from a complete bundle. Name is destination-local; the source is immutable.
type GaggleBundleImportRequest struct {
	Name   string       `json:"name"`
	Bundle GaggleBundle `json:"bundle"`
}

// GaggleBundleImportResult reports the created destination gaggle and retained
// source provenance.
type GaggleBundleImportResult struct {
	Name            string             `json:"name"`
	Source          GaggleBundleSource `json:"source"`
	ImportedAt      time.Time          `json:"importedAt"`
	RestartRequired bool               `json:"restartRequired"`
}
