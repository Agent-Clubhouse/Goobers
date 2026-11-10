package v1alpha1

// WorkflowGymSplit identifies how a benchmark task may be used.
type WorkflowGymSplit string

const (
	// WorkflowGymTaskAPIVersion is the apiVersion of Workflow Gym task manifests.
	WorkflowGymTaskAPIVersion = GroupName + "/" + Version
	// WorkflowGymTaskKind is the kind of Workflow Gym task manifests.
	WorkflowGymTaskKind = "WorkflowGymTask"
	// WorkflowGymReplayKind is the kind of Workflow Gym replay manifests.
	WorkflowGymReplayKind = "WorkflowGymReplay"

	// WorkflowGymSplitTrain marks a task usable for training.
	WorkflowGymSplitTrain WorkflowGymSplit = "train"
	// WorkflowGymSplitEval marks a task reserved for evaluation.
	WorkflowGymSplitEval WorkflowGymSplit = "eval"
	// WorkflowGymSplitHoldout marks a task withheld from training and routine evaluation.
	WorkflowGymSplitHoldout WorkflowGymSplit = "holdout"
)

// WorkflowGymEffectDisposition describes how an effect was handled offline.
type WorkflowGymEffectDisposition string

const (
	// WorkflowGymEffectDeterministic identifies recorded input replayed without
	// an external side effect.
	WorkflowGymEffectDeterministic WorkflowGymEffectDisposition = "deterministic"
	// WorkflowGymEffectVirtualized identifies an effect served by an in-memory
	// fixture boundary.
	WorkflowGymEffectVirtualized WorkflowGymEffectDisposition = "virtualized"
	// WorkflowGymEffectUnavailable identifies an effect refused by the offline
	// boundary because no fixture could serve it.
	WorkflowGymEffectUnavailable WorkflowGymEffectDisposition = "unavailable"
)

// WorkflowGymTaskManifest is one reproducible benchmark task in a versioned
// Workflow Gym dataset.
type WorkflowGymTaskManifest struct {
	APIVersion            string                          `json:"apiVersion" yaml:"apiVersion"`
	Kind                  string                          `json:"kind" yaml:"kind"`
	TaskID                string                          `json:"taskId" yaml:"taskId"`
	DatasetVersion        string                          `json:"datasetVersion" yaml:"datasetVersion"`
	Split                 WorkflowGymSplit                `json:"split" yaml:"split"`
	Provenance            WorkflowGymProvenance           `json:"provenance" yaml:"provenance"`
	Seed                  int64                           `json:"seed" yaml:"seed"`
	GeneratorVersion      string                          `json:"generatorVersion,omitempty" yaml:"generatorVersion,omitempty"`
	TaskClass             string                          `json:"taskClass,omitempty" yaml:"taskClass,omitempty"`
	Constraints           []string                        `json:"constraints,omitempty" yaml:"constraints,omitempty"`
	MinimalReproductionID string                          `json:"minimalReproductionId,omitempty" yaml:"minimalReproductionId,omitempty"`
	ExpectedOutcome       string                          `json:"expectedOutcome" yaml:"expectedOutcome"`
	ExpectedFailureMode   string                          `json:"expectedFailureMode,omitempty" yaml:"expectedFailureMode,omitempty"`
	EvaluatorVersion      string                          `json:"evaluatorVersion" yaml:"evaluatorVersion"`
	Retention             WorkflowGymRetention            `json:"retention" yaml:"retention"`
	Licensing             WorkflowGymLicensing            `json:"licensing" yaml:"licensing"`
	CandidateEnvironment  WorkflowGymCandidateEnvironment `json:"candidateEnvironment" yaml:"candidateEnvironment"`
}

// WorkflowGymProvenance identifies the immutable source material for a task.
type WorkflowGymProvenance struct {
	Source   string `json:"source" yaml:"source"`
	Revision string `json:"revision" yaml:"revision"`
}

// WorkflowGymRetention describes how long benchmark material may be retained.
type WorkflowGymRetention struct {
	Policy      string `json:"policy" yaml:"policy"`
	RetainUntil string `json:"retainUntil,omitempty" yaml:"retainUntil,omitempty"`
}

// WorkflowGymLicensing records the task material's license and attribution.
type WorkflowGymLicensing struct {
	License     string `json:"license" yaml:"license"`
	Attribution string `json:"attribution,omitempty" yaml:"attribution,omitempty"`
}

// WorkflowGymCandidateEnvironment pins the environment presented to candidates.
type WorkflowGymCandidateEnvironment struct {
	Image        string            `json:"image" yaml:"image"`
	OS           string            `json:"os,omitempty" yaml:"os,omitempty"`
	Architecture string            `json:"architecture,omitempty" yaml:"architecture,omitempty"`
	Environment  map[string]string `json:"environment,omitempty" yaml:"environment,omitempty"`
}

// WorkflowGymReplayManifest is the deterministic result of replaying one
// historical task through the offline benchmark harness.
type WorkflowGymReplayManifest struct {
	APIVersion             string                          `json:"apiVersion"`
	Kind                   string                          `json:"kind"`
	Candidate              WorkflowGymCandidateIdentity    `json:"candidate"`
	Environment            WorkflowGymCandidateEnvironment `json:"environment"`
	Sources                WorkflowGymReplaySources        `json:"sources"`
	Status                 string                          `json:"status"`
	JournalDigest          string                          `json:"journalDigest"`
	Effects                []WorkflowGymReplayEffect       `json:"effects"`
	VirtualizationGaps     []WorkflowGymReplayFinding      `json:"virtualizationGaps"`
	NondeterminismFindings []WorkflowGymReplayFinding      `json:"nondeterminismFindings"`
}

// WorkflowGymCandidateIdentity pins the candidate-facing task and evaluator.
type WorkflowGymCandidateIdentity struct {
	TaskID           string `json:"taskId"`
	DatasetVersion   string `json:"datasetVersion"`
	TaskDigest       string `json:"taskDigest"`
	EvaluatorVersion string `json:"evaluatorVersion"`
}

// WorkflowGymReplaySources identifies the historical material used by replay.
type WorkflowGymReplaySources struct {
	ProviderFixture string `json:"providerFixture"`
	Journal         string `json:"journal"`
}

// WorkflowGymReplayEffect records one deterministic, virtualized, or
// unavailable effect.
type WorkflowGymReplayEffect struct {
	Disposition WorkflowGymEffectDisposition `json:"disposition"`
	Effect      string                       `json:"effect"`
	Target      string                       `json:"target"`
	Detail      string                       `json:"detail,omitempty"`
}

// WorkflowGymReplayFinding is a stable replay diagnostic.
type WorkflowGymReplayFinding struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}
