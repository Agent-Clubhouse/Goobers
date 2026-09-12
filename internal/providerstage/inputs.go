package providerstage

import (
	"cmp"
	"slices"
)

// InputType describes the value shape a built-in provider stage reads from
// Task.Inputs. It is metadata for authoring and validation; runtime parsing
// remains the final defensive boundary for values that arrive in an envelope.
type InputType string

const (
	// InputString identifies a scalar string input.
	InputString InputType = "string"
	// InputBoolean identifies a boolean input.
	InputBoolean InputType = "boolean"
	// InputInteger identifies an integer input.
	InputInteger InputType = "integer"
	// InputDuration identifies a duration string input.
	InputDuration InputType = "duration"
	// InputStringList identifies a list of string values.
	InputStringList InputType = "string-list"
	// InputPath identifies a filesystem path input.
	InputPath InputType = "path"
)

// InputState records whether a declared provider-stage input can still be
// used. Retired inputs remain in the registry permanently so configuration
// validation can give an upgrade instruction instead of allowing a runtime
// failure.
type InputState string

const (
	// InputCurrent marks an input as accepted by current provider stages.
	InputCurrent InputState = "current"
	// InputRetired marks an input as rejected with migration guidance.
	InputRetired InputState = "retired"
)

// Input describes one workflow-supplied input consumed by a built-in provider
// stage. SinceDSL and UntilDSL are optional DSL-version bounds for the
// configuration surface. RetiredSince is a release or date recording a
// retirement that applies to every supported DSL version; Replacement must
// explain how an author should migrate.
type Input struct {
	Name         string
	Type         InputType
	State        InputState
	SinceDSL     string
	UntilDSL     string
	RetiredSince string
	Replacement  string
}

// inputSchemas is the complete contract for workflow-callable built-ins. Map
// presence distinguishes those closed schemas (including explicit empty
// schemas) from truly external commands, whose inputs remain open.
var inputSchemas = map[string][]Input{
	"__demo-provider": schema(
		stringsIn("baseSHA", "headSHA", "itemID", "itemTitle", "pullNumber", "pullRequestURL", "verdict"), pathsIn("resultFile"),
	),
	"apply-verdict": schema(
		stringsIn("base", "electionPolicy", "headPrefix", "reviewDigest", "selectedBaseSha", "selectedHeadSha", "siblingSerialization"),
		integersIn("selectedNumber"),
		booleansIn("advisoryMode", "publishAdvisory", "scopeGateParked"),
		stringListsIn("overlappingSiblings"), pathsIn("resultFile"), durationsIn("timeout"),
	),
	"backlog-assignment": schema(
		stringsIn("fieldPredicate", "labelPredicate", "strategy", "trustLabel"),
		integersIn("maxItems"), stringListsIn("excludeLabels", "requireLabels", "roster"),
		pathsIn("resultFile"), durationsIn("timeout"),
	),
	"backlog-dedupe": schema(integersIn("maxCandidates"), pathsIn("resultFile"), durationsIn("timeout")),
	"backlog-health": schema(
		stringsIn("readyLabel", "trustLabel"),
		integersIn("implementationFailureThreshold", "transitionScanMaxPages", "transitionScanQuotaFloor"),
		stringListsIn("partitionLabels", "requireLabels"), pathsIn("resultFile"), durationsIn("timeout"),
	),
	"backlog-query": {
		{Name: "assignedTo", Type: InputString, State: InputCurrent},
		{Name: "contestedFileMinPRs", Type: InputInteger, State: InputCurrent},
		{Name: "curation", Type: InputBoolean, State: InputCurrent},
		{Name: "deprioritizeContestedFiles", Type: InputBoolean, State: InputCurrent},
		{Name: "excludeLabels", Type: InputStringList, State: InputCurrent},
		{Name: "fieldOrder", Type: InputString, State: InputCurrent},
		{Name: "fieldPredicate", Type: InputString, State: InputCurrent},
		{Name: "filterParkLabels", Type: InputBoolean, State: InputCurrent},
		{Name: "labelPredicate", Type: InputString, State: InputCurrent},
		{Name: "leaseDuration", Type: InputDuration, State: InputCurrent},
		{Name: "maxItems", Type: InputInteger, State: InputCurrent},
		{Name: "parkLabels", Type: InputStringList, State: InputCurrent},
		{Name: "reconcileMetadata", Type: InputBoolean, State: InputCurrent},
		{Name: "requireLabels", Type: InputStringList, State: InputCurrent},
		{Name: "respectAssignee", Type: InputBoolean, State: InputCurrent},
		{Name: "resultFile", Type: InputPath, State: InputCurrent},
		{
			Name:         "resweepInterval",
			Type:         InputDuration,
			State:        InputRetired,
			RetiredSince: "2026-09-08",
			Replacement:  "configure schedule and readiness on a separate workflow using backlog-query --claim --resweep",
		},
		{Name: "resweepMaxItems", Type: InputInteger, State: InputCurrent},
		{Name: "resweepReadyLabel", Type: InputString, State: InputCurrent},
		{Name: "selectionPriority", Type: InputStringList, State: InputCurrent},
		{Name: "staleAfterDays", Type: InputInteger, State: InputCurrent},
		{Name: "staleAutoClose", Type: InputBoolean, State: InputCurrent},
		{Name: "trustLabel", Type: InputString, State: InputCurrent},
		{Name: "timeout", Type: InputDuration, State: InputCurrent},
	},
	"cancel-pending-ci": schema(
		stringsIn("headSha"), integersIn("maxRuns", "pullNumber"), pathsIn("resultFile"), durationsIn("timeout"),
	),
	"check-fail-first": schema(stringsIn("base"), pathsIn("evidenceFile")),
	"check-issue-staleness": schema(
		stringsIn("base", "head"), integersIn("pullNumber"), booleansIn("advisoryMode"),
		pathsIn("resultFile"), durationsIn("timeout"),
	),
	"docs-churn": schema(
		stringsIn("sinceFloor"), booleansIn("advanceWatermark"), integersIn("bufferMultiplier"),
		stringListsIn("docsRoots"), pathsIn("resultFile"),
	),
	"elect-lander": schema(
		stringsIn("base", "electionPolicy", "headPrefix", "reviewDigest", "selectedBaseSha", "selectedHeadSha", "siblingSerialization"),
		integersIn("selectedNumber"), booleansIn("advisoryMode", "scopeGateParked"),
		stringListsIn("overlappingSiblings"), pathsIn("resultFile"), durationsIn("timeout"),
	),
	"file-issues": schema(
		stringsIn("autoApprove", "backlogLabel", "checkDigest", "checkFile", "checkStage", "nominatedLabel", "nominationsFile", "partitionLabel", "producerStage", "signalsStage"),
		integersIn("dedupeWindowDays", "maxPerRun"), pathsIn("resultFile"), durationsIn("timeout"),
	),
	"gather-ci-failures":       schema(pathsIn("resultFile"), durationsIn("timeout")),
	"gather-implement-context": schema(stringsIn("base"), integersIn("maxHotFiles"), pathsIn("resultFile"), durationsIn("timeout")),
	"gather-issue-context":     schema(pathsIn("resultFile"), durationsIn("timeout")),
	"gather-pr-context": schema(
		stringsIn("base", "headPrefix", "minSeverity", "remediationAlgorithm", "selectedNumber"),
		pathsIn("resultFile"), durationsIn("timeout"),
	),
	"gather-review-threads": schema(pathsIn("resultFile"), durationsIn("timeout")),
	"gather-sibling-context": schema(
		stringsIn("authorScope", "base", "head", "minSeverity"),
		integersIn("scopeDriftThreshold", "scopeGateFilesThreshold", "scopeGateLinesThreshold", "selectedNumber"),
		booleansIn("advisoryMode", "hasFailingCI", "hasSubstantiveFindings"),
		stringListsIn("headPrefixes"),
		pathsIn("resultFile"), durationsIn("timeout"),
	),
	"gate-removal-guard": schema(stringsIn("base"), pathsIn("resultFile")),
	"issue-close-out": schema(
		stringsIn("base", "comment", "head", "reason", "reasonFromGate", "status"), pathsIn("resultFile"), durationsIn("timeout"),
	),
	"merge-pr": schema(
		stringsIn("baseSha", "commitMessage", "headSha", "mergeMethod", "verdict", "verdictAuthor"),
		integersIn("pullNumber"), booleansIn("advisoryMode"), pathsIn("resultFile"), durationsIn("timeout"),
	),
	"merge-queue-poll": schema(
		integersIn("pullNumber"), durationsIn("pollIntervalSeconds", "pollMaxIntervalSeconds", "pollTimeoutSeconds", "timeout"), pathsIn("resultFile"),
	),
	"open-pr": schema(
		stringsIn("base", "body", "configRoot", "head", "title", "tutorConfigSource"),
		booleansIn("confineToActionRoots", "confineToConfigRoot", "confineToDocsRoots", "recordLiveVerification", "runIdFooter"),
		stringListsIn("actionRoots", "docsRoots"), pathsIn("resultFile"), durationsIn("timeout"),
	),
	"post-merge": schema(integersIn("pullNumber"), pathsIn("resultFile"), durationsIn("timeout")),
	"pr-claim":   schema(durationsIn("leaseDuration", "timeout"), pathsIn("resultFile")),
	"pr-comment-watch": schema(
		stringsIn("base"), integersIn("maxPullRequests"),
		stringListsIn("excludeAuthors", "excludeLabels", "headPrefixes", "unparkLabels"),
		pathsIn("resultFile"), durationsIn("timeout"),
	),
	"pr-select": schema(
		stringsIn("assignee", "author", "authorScope", "base", "headPrefix", "requestedReviewer", "requireOptInLabel", "selfIdentity"),
		booleansIn("allowPendingChecks", "respectAssignee"), stringListsIn("excludeLabels", "headPrefixes"),
		pathsIn("resultFile"), durationsIn("timeout"),
	),
	"preflight-repo-write": schema(stringsIn("branch"), durationsIn("timeout")),
	"publish-batch":        schema(stringsIn("planFile", "validationFile"), pathsIn("resultFile"), durationsIn("timeout")),
	"push-remediated":      schema(pathsIn("resultFile"), durationsIn("timeout")),
	"rebase-pr": schema(
		stringsIn("base", "head", "remediate", "selectedNumber"),
		booleansIn("hasFailingCI", "hasSiblingOverlap", "hasSubstantiveFindings"),
		pathsIn("resultFile"), durationsIn("timeout"),
	),
	"reconcile-branches": schema(
		stringsIn("after"), integersIn("maxBranches"), booleansIn("deleteBranches"),
		durationsIn("minimumAge", "timeout"), pathsIn("resultFile"),
	),
	"reconcile-post-merge": schema(
		stringsIn("base"), integersIn("maxPullRequests"), durationsIn("lookback", "timeout"), pathsIn("resultFile"),
	),
	"record-merge-refusal": schema(
		stringsIn("reason", "selectedHeadSha"), integersIn("demotionThreshold", "selectedNumber"), durationsIn("timeout"),
	),
	"remediation-checkpoint": schema(
		stringsIn("attemptedHeadSha", "base", "conflictLocations", "headPrefix", "policyExcludedReason", "rebaseBaseSha", "remediationCauses", "selectedNumber"),
		integersIn("conflictBudget", "failingCIBudget", "humanCommentBudget", "siblingOverlapBudget", "substantiveBudget"),
		booleansIn("conflict", "policyExcluded", "rebaseInfrastructureFailure"),
		pathsIn("resultFile"), durationsIn("timeout"),
	),
	"report-pr-status": schema(
		stringsIn("description", "pull-request-url", "state", "statusGenre", "statusName", "targetUrl"),
		integersIn("prNumber"), pathsIn("resultFile"), durationsIn("timeout"),
	),
	"resolve-review-threads": schema(pathsIn("resultFile"), durationsIn("timeout")),
	"respond-to-findings":    schema(stringsIn("implementStage", "pushStage"), pathsIn("resultFile"), durationsIn("timeout")),
	"security-alerts-query": schema(
		stringsIn("ref", "scope", "source", "state", "tool"), stringListsIn("ecosystem", "severity"),
		integersIn("maxResults"), pathsIn("resultFile"), durationsIn("timeout"),
	),
	"select-source": schema(stringsIn("trustLabel"), durationsIn("leaseDuration", "timeout"), pathsIn("resultFile")),
	"self-update": schema(
		stringsIn("branch", "owner", "policy", "repository", "target"), pathsIn("resultFile"),
	),
	"set-milestone":    schema(stringsIn("itemID", "milestone"), pathsIn("resultFile"), durationsIn("timeout")),
	"telemetry-query":  schema(pathsIn("resultFile")),
	"update-behind-pr": schema(stringsIn("base", "headPrefix", "minSeverity"), pathsIn("resultFile"), durationsIn("timeout")),
	"validate-plan":    schema(stringsIn("planFile", "selectionFile"), pathsIn("resultFile"), durationsIn("timeout")),

	// These provider commands read no workflow-supplied inputs today. Keeping
	// an explicit empty schema distinguishes them from external/unknown
	// commands and makes a newly added consumer fail the structural parity
	// test until its contract is declared here.
	"push-branch":        {},
	"recovery-restore":   {},
	"recovery-resume":    {},
	"ios-simulator-test": schema(integersIn("maxOutputBytes"), pathsIn("resultFile")),
	"mcp-io":             {},
	"validate":           {},
}

// executorInputs are valid for every deterministic shell stage. They are
// consumed by the shell executor before the built-in command starts, so they
// belong to the effective contract of every built-in rather than to any one
// command implementation.
var executorInputs = []Input{
	{Name: "maxOutputBytes", Type: InputInteger, State: InputCurrent},
	{Name: "timeout", Type: InputDuration, State: InputCurrent},
}

func schema(groups ...[]Input) []Input {
	var inputs []Input
	for _, group := range groups {
		inputs = append(inputs, group...)
	}
	return inputs
}

func currentInputs(kind InputType, names ...string) []Input {
	inputs := make([]Input, 0, len(names))
	for _, name := range names {
		inputs = append(inputs, Input{Name: name, Type: kind, State: InputCurrent})
	}
	return inputs
}

func stringsIn(names ...string) []Input     { return currentInputs(InputString, names...) }
func booleansIn(names ...string) []Input    { return currentInputs(InputBoolean, names...) }
func integersIn(names ...string) []Input    { return currentInputs(InputInteger, names...) }
func durationsIn(names ...string) []Input   { return currentInputs(InputDuration, names...) }
func stringListsIn(names ...string) []Input { return currentInputs(InputStringList, names...) }
func pathsIn(names ...string) []Input       { return currentInputs(InputPath, names...) }

// InputSchemaForVersion resolves command's declared inputs at one DSL version
// and reports whether command is a known workflow-callable built-in. Unbounded
// entries apply to every version; bounded entries use the same
// inclusive-since/exclusive-until semantics as capability requirements.
// Callers use ok to keep external commands open while treating a built-in's
// declared schema as complete.
func InputSchemaForVersion(command, dslVersion string) ([]Input, bool) {
	declared, ok := inputSchemas[command]
	if !ok {
		return nil, false
	}
	byName := make(map[string]Input, len(executorInputs)+len(declared))
	for _, input := range append(append([]Input(nil), executorInputs...), declared...) {
		if activeAt(dslVersion, input.SinceDSL, input.UntilDSL) {
			// A command-specific declaration intentionally overrides common
			// metadata for the same input name.
			byName[input.Name] = input
		}
	}
	inputs := make([]Input, 0, len(byName))
	for _, input := range byName {
		inputs = append(inputs, input)
	}
	slices.SortFunc(inputs, func(a, b Input) int { return cmp.Compare(a.Name, b.Name) })
	return inputs, true
}
