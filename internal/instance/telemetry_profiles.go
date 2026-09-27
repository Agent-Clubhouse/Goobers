package instance

// TelemetryCollectionProfile selects the versioned tenant-export signal set.
// Local journals, rollups, and diagnostic history are unaffected.
type TelemetryCollectionProfile string

const (
	// TelemetryProfileHealth exports only bounded service and fleet health.
	TelemetryProfileHealth TelemetryCollectionProfile = "health"
	// TelemetryProfileJournal adds committed structured journal records.
	TelemetryProfileJournal TelemetryCollectionProfile = "journal"
	// TelemetryProfileStandard adds correlated run and stage traces.
	TelemetryProfileStandard TelemetryCollectionProfile = "standard"
	// TelemetryProfileDiagnostic additionally permits host/account identity.
	TelemetryProfileDiagnostic TelemetryCollectionProfile = "diagnostic"

	// TelemetryCollectionProfileContractVersion identifies the public v1 contract.
	TelemetryCollectionProfileContractVersion = "goobers.dev/telemetry/collection-profiles/v1"
)

// EffectiveCollectionProfile preserves the signal set shipped before profiles:
// one configured Azure Monitor destination receives health, journal, and trace
// records. Host/account identity is now an additional explicit consent choice.
func (c TelemetryConfig) EffectiveCollectionProfile() TelemetryCollectionProfile {
	if c.CollectionProfile == "" {
		return TelemetryProfileStandard
	}
	return c.CollectionProfile
}

func (p TelemetryCollectionProfile) valid() bool {
	for _, profile := range TelemetryCollectionContract().Profiles {
		if profile.Name == p {
			return true
		}
	}
	return false
}

// IncludesJournal reports whether committed structured journal records belong
// to this profile's tenant-export signal set.
func (p TelemetryCollectionProfile) IncludesJournal() bool {
	return p == TelemetryProfileJournal || p == TelemetryProfileStandard || p == TelemetryProfileDiagnostic
}

// IncludesTraces reports whether correlated run/stage spans belong to this
// profile's tenant-export signal set.
func (p TelemetryCollectionProfile) IncludesTraces() bool {
	return p == TelemetryProfileStandard || p == TelemetryProfileDiagnostic
}

// IncludesHostIdentity reports whether machine and runtime-account identity
// may leave the instance. It is deliberately false for the default profile.
func (p TelemetryCollectionProfile) IncludesHostIdentity() bool {
	return p == TelemetryProfileDiagnostic
}

// TelemetryCollectionProfileDescriptor is the machine-readable public
// contract used to generate the checked-in reference document.
type TelemetryCollectionProfileDescriptor struct {
	Name                TelemetryCollectionProfile `json:"name"`
	Health              bool                       `json:"health"`
	Journal             bool                       `json:"journal"`
	Traces              bool                       `json:"traces"`
	HostAccountIdentity bool                       `json:"hostAccountIdentity"`
}

// TelemetryCollectionProfileContract is the versioned, generated operator
// reference. Structured journal bodies remain JSON; profiles never base64-wrap
// workflows, credentials, prompts, or arbitrary payloads.
type TelemetryCollectionProfileContract struct {
	Schema                    string                                 `json:"schema"`
	DefaultProfile            TelemetryCollectionProfile             `json:"defaultProfile"`
	ProfileField              string                                 `json:"profileField"`
	StructuredPayloadEncoding string                                 `json:"structuredPayloadEncoding"`
	CorrelationFields         []string                               `json:"correlationFields"`
	ConsentGatedIdentity      []string                               `json:"consentGatedIdentity"`
	Profiles                  []TelemetryCollectionProfileDescriptor `json:"profiles"`
}

// TelemetryCollectionProfiles returns the ordered v1 collection contract.
func TelemetryCollectionProfiles() []TelemetryCollectionProfileDescriptor {
	profiles := []TelemetryCollectionProfile{
		TelemetryProfileHealth,
		TelemetryProfileJournal,
		TelemetryProfileStandard,
		TelemetryProfileDiagnostic,
	}
	result := make([]TelemetryCollectionProfileDescriptor, 0, len(profiles))
	for _, profile := range profiles {
		result = append(result, TelemetryCollectionProfileDescriptor{
			Name: profile, Health: true, Journal: profile.IncludesJournal(),
			Traces: profile.IncludesTraces(), HostAccountIdentity: profile.IncludesHostIdentity(),
		})
	}
	return result
}

// TelemetryCollectionContract returns the complete v1 public contract.
func TelemetryCollectionContract() TelemetryCollectionProfileContract {
	return TelemetryCollectionProfileContract{
		Schema:                    TelemetryCollectionProfileContractVersion,
		DefaultProfile:            TelemetryProfileStandard,
		ProfileField:              "goobers.telemetry.profile",
		StructuredPayloadEncoding: "json",
		CorrelationFields: []string{
			"goobers.instance.id", "goobers.gaggle", "goobers.workflow",
			"goobers.run.id", "trace_id", "span_id",
		},
		ConsentGatedIdentity: []string{"machineName", "accountName", "ai.device.id", "ai.cloud.roleInstance"},
		Profiles:             TelemetryCollectionProfiles(),
	}
}
