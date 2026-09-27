package instance

// TenantObservabilityContractVersion identifies the public incident and
// correlation contract implemented by the direct tenant exporter.
const TenantObservabilityContractVersion = "goobers.dev/telemetry/tenant-observability/v1"

// TelemetryCadenceDescriptor documents a bounded recurring signal. Records
// caused by state transitions may be emitted sooner than the periodic bound.
type TelemetryCadenceDescriptor struct {
	Signal                  string `json:"signal"`
	PeriodicSeconds         int    `json:"periodicSeconds"`
	TransitionWithinSeconds int    `json:"transitionWithinSeconds,omitempty"`
}

// TelemetryBoundDescriptor documents a hard observation or delivery bound.
type TelemetryBoundDescriptor struct {
	Name  string `json:"name"`
	Value int64  `json:"value"`
	Unit  string `json:"unit"`
}

// TelemetryIncidentDescriptor is the stable evidence recipe for an incident
// class. Required fields are correlation keys rather than arbitrary payloads.
type TelemetryIncidentDescriptor struct {
	ID             string                     `json:"id"`
	Signal         string                     `json:"signal"`
	EventNames     []string                   `json:"eventNames"`
	StableCodes    []string                   `json:"stableCodes"`
	RequiredFields []string                   `json:"requiredFields"`
	MinimumProfile TelemetryCollectionProfile `json:"minimumProfile"`
}

// TenantObservabilityContract is the generated operator-facing v1 contract.
type TenantObservabilityContract struct {
	Schema                    string                        `json:"schema"`
	CollectionProfileSchema   string                        `json:"collectionProfileSchema"`
	Cadences                  []TelemetryCadenceDescriptor  `json:"cadences"`
	Bounds                    []TelemetryBoundDescriptor    `json:"bounds"`
	CorrelationFields         []string                      `json:"correlationFields"`
	Incidents                 []TelemetryIncidentDescriptor `json:"incidents"`
	ForbiddenFields           []string                      `json:"forbiddenFields"`
	StructuredPayloadEncoding string                        `json:"structuredPayloadEncoding"`
}

// TenantTelemetryContract returns the complete v1 incident-visibility
// contract. Keep this vocabulary small: operators can alert on these values,
// so changing one requires a new version rather than an in-place rename.
func TenantTelemetryContract() TenantObservabilityContract {
	return TenantObservabilityContract{
		Schema:                  TenantObservabilityContractVersion,
		CollectionProfileSchema: TelemetryCollectionProfileContractVersion,
		Cadences: []TelemetryCadenceDescriptor{
			{Signal: "goobers.fleet.heartbeat/deployment", PeriodicSeconds: 60},
			{Signal: "goobers.fleet.heartbeat/gaggle", PeriodicSeconds: 60, TransitionWithinSeconds: 60},
			{Signal: "goobers.service.health", PeriodicSeconds: 21600},
		},
		Bounds: []TelemetryBoundDescriptor{
			{Name: "inventoryPage", Value: 100, Unit: "items"},
			{Name: "inventoryObservation", Value: 1000, Unit: "items"},
			{Name: "retainedRunsPerGaggle", Value: 100, Unit: "runs"},
			{Name: "diagnosticBatch", Value: 128, Unit: "records"},
			{Name: "journalMemoryQueue", Value: 1024, Unit: "records"},
			{Name: "journalMemoryQueueBytes", Value: 8388608, Unit: "bytes"},
			{Name: "azureReplayMaxAge", Value: 72, Unit: "hours"},
			{Name: "azureReplayMaxBytes", Value: 536870912, Unit: "bytes"},
		},
		CorrelationFields: []string{
			"goobers.instance.id", "instanceId", "bootId", "goobers.gaggle", "gaggleId",
			"goobers.workflow", "goobers.workflow.version", "goobers.workflow.digest",
			"goobers.config.generation", "goobers.trigger.kind", "goobers.run.id", "trace_id", "span_id",
			"goobers.stage", "goobers.attempt.n", "goobers.telemetry.record_id",
		},
		Incidents: []TelemetryIncidentDescriptor{
			{ID: "pat_auth_rejection", Signal: "journal", EventNames: []string{"stage.finished"}, StableCodes: []string{"github_auth_failed"}, RequiredFields: []string{"goobers.instance.id", "goobers.gaggle", "goobers.workflow", "goobers.run.id", "goobers.stage"}, MinimumProfile: TelemetryProfileJournal},
			{ID: "credential_unavailable", Signal: "journal", EventNames: []string{"stage.finished"}, StableCodes: []string{"credential_unavailable"}, RequiredFields: []string{"goobers.instance.id", "goobers.gaggle", "goobers.workflow", "goobers.run.id", "goobers.stage"}, MinimumProfile: TelemetryProfileJournal},
			{ID: "startup_not_ready", Signal: "diagnostic", EventNames: []string{"goobers.fleet.heartbeat"}, StableCodes: []string{"startup"}, RequiredFields: []string{"instanceId", "bootId", "state", "reasonCode", "windowCoverage"}, MinimumProfile: TelemetryProfileHealth},
			{ID: "workflow_refused", Signal: "journal", EventNames: []string{"workflow.refused"}, StableCodes: []string{"conditions: harness-unavailable"}, RequiredFields: []string{"goobers.instance.id", "goobers.gaggle", "goobers.workflow"}, MinimumProfile: TelemetryProfileJournal},
			{ID: "provider_api_failure", Signal: "journal", EventNames: []string{"stage.finished"}, StableCodes: []string{"provider_error", "poll_provider_error", "github_rate_limited"}, RequiredFields: []string{"goobers.instance.id", "goobers.gaggle", "goobers.workflow", "goobers.run.id", "goobers.stage"}, MinimumProfile: TelemetryProfileJournal},
			{ID: "harness_startup_failure", Signal: "journal", EventNames: []string{"workflow.refused", "stage.finished"}, StableCodes: []string{"conditions: harness-unavailable", "harness.failure"}, RequiredFields: []string{"goobers.instance.id", "goobers.gaggle", "goobers.workflow"}, MinimumProfile: TelemetryProfileJournal},
			{ID: "stuck_run", Signal: "diagnostic", EventNames: []string{"goobers.fleet.heartbeat"}, StableCodes: []string{"no_progress"}, RequiredFields: []string{"instanceId", "gaggleId", "state", "reasonCode", "windowCoverage"}, MinimumProfile: TelemetryProfileHealth},
			{ID: "incomplete_observation", Signal: "diagnostic", EventNames: []string{"goobers.fleet.heartbeat"}, StableCodes: []string{"observation_incomplete"}, RequiredFields: []string{"instanceId", "state", "reasonCode", "windowCoverage"}, MinimumProfile: TelemetryProfileHealth},
			{ID: "exporter_loss_or_recovery", Signal: "diagnostic", EventNames: []string{"goobers.service.health", "goobers.fleet.heartbeat"}, StableCodes: []string{"azure_replay_pending", "azure_replay_pruned", "azure_replay_malformed"}, RequiredFields: []string{"instanceId", "goobers.telemetry.record_id", "azureReplayPendingRecords", "azureReplayRetried"}, MinimumProfile: TelemetryProfileHealth},
		},
		ForbiddenFields: []string{
			"authorization", "connectionString", "credentialValue", "password", "patValue",
			"secretValue", "tokenValue", "environmentValue", "rawConfig", "rawPrompt", "rawWorkflow", "workflowBase64",
		},
		StructuredPayloadEncoding: "json",
	}
}
