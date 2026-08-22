package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/api/schemas"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

const candidateFindingsSchemaVersion = "goobers.dev/candidate-findings/v1"

type candidateFindingsArtifact struct {
	Schema   string           `json:"schema"`
	Window   string           `json:"window"`
	Since    time.Time        `json:"since"`
	Findings []rollup.Finding `json:"findings"`
	NoWork   bool             `json:"noWork,omitempty"`
	Note     string           `json:"note,omitempty"`
}

const (
	telemetryQueryNoRollupNote                   = "no telemetry rollup yet"
	telemetryQueryNoFindingsNote                 = "telemetry rollup has no candidate findings in the requested window"
	telemetryQueryNoEffectiveVersionChangeNote   = "workflow has no observed EffectiveVersion transition"
	telemetryQueryCandidateFormat                = "candidate-findings"
	telemetryQueryEffectiveVersionEfficacyFormat = "effective-version-efficacy"
	telemetryQueryTutorHoldoutsFormat            = "tutor-live-verification"
)

const effectiveVersionEfficacySchemaVersion = "goobers.dev/effective-version-efficacy/v1"

// effectiveVersionEfficacyArtifact is the --format effective-version-efficacy
// wire artifact: whether a workflow's most recent EffectiveVersion
// transition (the version-segmented cohort key from the Tutor v2 redesign,
// docs/design/tutor-redesign.md §4.1) helped or regressed. OldVersion/
// NewVersion are nil when the workflow has no observed transition.
type effectiveVersionEfficacyArtifact struct {
	Schema           string                   `json:"schema"`
	Workflow         string                   `json:"workflow"`
	Window           string                   `json:"window"`
	Since            time.Time                `json:"since"`
	OldVersion       *rollup.EffectiveVersion `json:"oldVersion,omitempty"`
	NewVersion       *rollup.EffectiveVersion `json:"newVersion,omitempty"`
	Verdict          rollup.EfficacyVerdict   `json:"verdict"`
	FailureRateDelta float64                  `json:"failureRateDelta"`
	Before           rollup.RunStats          `json:"before"`
	After            rollup.RunStats          `json:"after"`
	NoWork           bool                     `json:"noWork,omitempty"`
	Note             string                   `json:"note,omitempty"`
}

func newEffectiveVersionEfficacyArtifact(window time.Duration, since time.Time, workflow string, result rollup.EffectiveVersionEfficacyResult) effectiveVersionEfficacyArtifact {
	artifact := effectiveVersionEfficacyArtifact{
		Schema:           effectiveVersionEfficacySchemaVersion,
		Workflow:         workflow,
		Window:           window.String(),
		Since:            since,
		Verdict:          result.Verdict,
		FailureRateDelta: result.FailureRateDelta,
		Before:           result.Before,
		After:            result.After,
	}
	if result.OldVersionHash == "" && result.NewVersionHash == "" {
		artifact.NoWork = true
		artifact.Note = telemetryQueryNoEffectiveVersionChangeNote
		return artifact
	}
	oldVersion := result.OldVersion
	newVersion := result.NewVersion
	artifact.OldVersion = &oldVersion
	artifact.NewVersion = &newVersion
	return artifact
}

type telemetryAggregate string

const (
	telemetryAggregateAll                 telemetryAggregate = "all"
	telemetryAggregateStageFailureRate    telemetryAggregate = "stage-failure-rate"
	telemetryAggregateErrorSignature      telemetryAggregate = "error-signature"
	telemetryAggregateCICheckFailure      telemetryAggregate = "ci-check-failure"
	telemetryAggregateGateNoise           telemetryAggregate = "gate-noise"
	telemetryAggregateWorkflowUntriggered telemetryAggregate = "workflow-untriggered"
	telemetryAggregateStageUnreached      telemetryAggregate = "stage-unreached"
	telemetryAggregateCreditAssignment    telemetryAggregate = "credit-assignment"
)

type telemetryAggregateValues []telemetryAggregate

func (v *telemetryAggregateValues) String() string {
	values := make([]string, len(*v))
	for i, aggregate := range *v {
		values[i] = string(aggregate)
	}
	return strings.Join(values, ",")
}

func (v *telemetryAggregateValues) Set(raw string) error {
	var aggregate telemetryAggregate
	switch raw {
	case string(telemetryAggregateAll):
		aggregate = telemetryAggregateAll
	case string(telemetryAggregateStageFailureRate), "failure-rate":
		aggregate = telemetryAggregateStageFailureRate
	case string(telemetryAggregateErrorSignature), "error-signatures":
		aggregate = telemetryAggregateErrorSignature
	case string(telemetryAggregateCICheckFailure):
		aggregate = telemetryAggregateCICheckFailure
	case string(telemetryAggregateGateNoise):
		aggregate = telemetryAggregateGateNoise
	case string(telemetryAggregateWorkflowUntriggered):
		aggregate = telemetryAggregateWorkflowUntriggered
	case string(telemetryAggregateStageUnreached):
		aggregate = telemetryAggregateStageUnreached
	case string(telemetryAggregateCreditAssignment):
		aggregate = telemetryAggregateCreditAssignment
	default:
		return fmt.Errorf("unknown aggregate %q (allowed: all, stage-failure-rate, error-signature, ci-check-failure, gate-noise, workflow-untriggered, stage-unreached, credit-assignment)", raw)
	}
	for _, existing := range *v {
		if existing == aggregate {
			return nil
		}
	}
	*v = append(*v, aggregate)
	return nil
}

func (v telemetryAggregateValues) includes(kind rollup.FindingKind) bool {
	if len(v) == 0 {
		return true
	}
	for _, aggregate := range v {
		switch aggregate {
		case telemetryAggregateAll:
			return true
		case telemetryAggregateStageFailureRate:
			if kind == rollup.FindingStageFailureRate {
				return true
			}
		case telemetryAggregateErrorSignature:
			if kind == rollup.FindingErrorSignature {
				return true
			}
		case telemetryAggregateCICheckFailure:
			if kind == rollup.FindingCICheckFailure {
				return true
			}
		case telemetryAggregateGateNoise:
			if kind == rollup.FindingGateNeverFails || kind == rollup.FindingGateRepassChurn {
				return true
			}
		case telemetryAggregateWorkflowUntriggered:
			if kind == rollup.FindingWorkflowUntriggered {
				return true
			}
		case telemetryAggregateStageUnreached:
			if kind == rollup.FindingStageUnreached {
				return true
			}
		case telemetryAggregateCreditAssignment:
			if kind == rollup.FindingCreditAssignment {
				return true
			}
		}
	}
	return false
}

type telemetryThresholdValue struct {
	thresholds *rollup.Thresholds
}

func (v *telemetryThresholdValue) String() string {
	return ""
}

func (v *telemetryThresholdValue) Set(raw string) error {
	key, value, ok := strings.Cut(raw, "=")
	if !ok || strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
		return fmt.Errorf("threshold must be k=v, got %q", raw)
	}
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)

	parsePositiveInt := func() (int, error) {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 {
			return 0, fmt.Errorf("%s must be a positive integer", key)
		}
		return n, nil
	}
	parseRate := func() (float64, error) {
		rate, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 || rate > 1 {
			return 0, fmt.Errorf("%s must be a number between 0 and 1", key)
		}
		return rate, nil
	}

	switch key {
	case "min-samples", "minSamples":
		n, err := parsePositiveInt()
		if err != nil {
			return err
		}
		v.thresholds.MinSamples = n
	case "max-failure-rate", "maxFailureRate":
		rate, err := parseRate()
		if err != nil {
			return err
		}
		v.thresholds.MaxFailureRate = rate
	case "min-error-signature-count", "minErrorSignatureCount":
		n, err := parsePositiveInt()
		if err != nil {
			return err
		}
		v.thresholds.MinErrorSignatureCount = n
	case "min-ci-check-failure-runs", "minCICheckFailureRuns":
		n, err := parsePositiveInt()
		if err != nil {
			return err
		}
		v.thresholds.MinCICheckFailureRuns = n
	case "min-gate-evaluations", "minGateEvaluations":
		n, err := parsePositiveInt()
		if err != nil {
			return err
		}
		v.thresholds.MinGateEvaluations = n
	case "max-gate-escalation-rate", "maxGateEscalationRate":
		rate, err := parseRate()
		if err != nil {
			return err
		}
		v.thresholds.MaxGateEscalationRate = rate
	case "max-flagged-runs", "maxFlaggedRuns":
		n, err := parsePositiveInt()
		if err != nil {
			return err
		}
		v.thresholds.MaxFlaggedRuns = n
	case "min-credit-runs", "minCreditRuns":
		n, err := parsePositiveInt()
		if err != nil {
			return err
		}
		v.thresholds.MinCreditRuns = n
	case "min-credit-failure-share", "minCreditFailureShare":
		rate, err := parseRate()
		if err != nil {
			return err
		}
		v.thresholds.MinCreditFailureShare = rate
	default:
		return fmt.Errorf("unknown threshold %q", key)
	}
	return nil
}

const telemetryQueryHelp = "Usage: goobers telemetry-query [--window <duration>] [--aggregate <name>]... [--threshold <k=v>]... [--format candidate-findings|effective-version-efficacy|tutor-live-verification] [--gaggle <name>] [--workflow <name>] [path]\n\n" +
	"Query the instance telemetry rollup for threshold-crossing failure and gate\n" +
	"patterns. The built-in connector stage writes a versioned candidate-findings\n" +
	"artifact to GOOBERS_INPUT_resultFile when declared, or to stdout otherwise.\n" +
	"With no --aggregate, all supported aggregates are evaluated. Threshold rates\n" +
	"are fractions from 0 through 1; count thresholds are positive integers.\n\n" +
	"--format effective-version-efficacy (requires --workflow) instead assesses\n" +
	"the workflow's most recent EffectiveVersion transition — the version-\n" +
	"segmented cohort key (workflow digest + goober digest + model + harness\n" +
	"version) from the Tutor v2 design — and emits a helped/regressed/no-change/\n" +
	"insufficient-data verdict. --format tutor-live-verification evaluates all\n" +
	"durable mandatory Tutor holdouts for the current gaggle from each PR's\n" +
	"merge time and first observed post-merge configuration transition. Exact\n" +
	"EffectiveVersion cohorts verify transitions and additions; a removal is\n" +
	"verified when the workflow is absent from the live reconciled config.\n\n" +
	"Exit codes: 0 = OK (including a clean no-work result), 1 = business error,\n" +
	"2 = usage/IO error.\n"

// runTelemetryQuery implements the deterministic telemetry connector stage.
// It locates the instance through GOOBERS_INSTANCE_ROOT because the stage's
// working directory is its isolated worktree, not the instance root.
func runTelemetryQuery(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("telemetry-query", flag.ContinueOnError)
	fs.SetOutput(stderr)
	window := fs.Duration("window", 24*time.Hour, "lookback window (for example 24h or 168h)")
	format := fs.String("format", telemetryQueryCandidateFormat, "artifact format (candidate-findings, effective-version-efficacy, tutor-live-verification)")
	gaggle := fs.String("gaggle", "", "gaggle to query (default $GOOBERS_GAGGLE)")
	workflow := fs.String("workflow", "", "workflow name (required for --format effective-version-efficacy)")
	var aggregates telemetryAggregateValues
	fs.Var(&aggregates, "aggregate", "aggregate to detect; repeat for multiple (all, stage-failure-rate, error-signature, ci-check-failure, gate-noise, workflow-untriggered, stage-unreached, credit-assignment)")
	thresholds := rollup.DefaultThresholds()
	fs.Var(&telemetryThresholdValue{thresholds: &thresholds}, "threshold",
		"threshold override k=v; repeat for multiple (min-samples, max-failure-rate, min-error-signature-count, min-ci-check-failure-runs, min-gate-evaluations, max-gate-escalation-rate, max-flagged-runs, min-credit-runs, min-credit-failure-share)")
	fs.Usage = helpUsage(stderr, "telemetry-query")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	if *window <= 0 {
		pf(stderr, "error: --window must be a positive duration, got %s\n", *window)
		return 2
	}
	if *format != telemetryQueryCandidateFormat &&
		*format != telemetryQueryEffectiveVersionEfficacyFormat &&
		*format != telemetryQueryTutorHoldoutsFormat {
		pf(stderr, "error: --format must be %q, %q, or %q, got %q\n",
			telemetryQueryCandidateFormat,
			telemetryQueryEffectiveVersionEfficacyFormat,
			telemetryQueryTutorHoldoutsFormat,
			*format,
		)
		return 2
	}
	if *format == telemetryQueryEffectiveVersionEfficacyFormat && strings.TrimSpace(*workflow) == "" {
		pf(stderr, "error: --format %s requires --workflow\n", telemetryQueryEffectiveVersionEfficacyFormat)
		return 2
	}
	pathArg := ""
	if fs.NArg() == 1 {
		pathArg = fs.Arg(0)
	}

	since := time.Now().UTC().Add(-*window)
	root := providerStageRoot(pathArg)
	queryGaggle := strings.TrimSpace(*gaggle)
	if queryGaggle == "" {
		queryGaggle = strings.TrimSpace(os.Getenv("GOOBERS_GAGGLE"))
	}
	if queryGaggle == "" && strings.TrimSpace(*workflow) != "" {
		var err error
		queryGaggle, err = resolveTelemetryQueryGaggle(root, *workflow)
		if err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
	}
	if *format == telemetryQueryTutorHoldoutsFormat {
		if err := refreshTutorHoldoutMergeStateFromProvider(root, queryGaggle); err != nil {
			pf(stderr, "error: refresh Tutor live holdout merge state: %v\n", err)
			return 1
		}
	}
	l := layoutFor(root)
	dbPath := l.TelemetryDB()
	if _, err := os.Stat(dbPath); err != nil {
		if !os.IsNotExist(err) {
			pf(stderr, "error: inspect telemetry rollup %s: %v\n", dbPath, err)
			return 1
		}
		pf(stderr, "note: no telemetry rollup at %s — if this persists, enable telemetry "+
			"(instance.yaml telemetry.enabled) and run at least one workflow under `goobers up`: %v\n", dbPath, err)
		if *format == telemetryQueryEffectiveVersionEfficacyFormat {
			result := newEffectiveVersionEfficacyArtifact(*window, since, *workflow,
				rollup.EffectiveVersionEfficacyResult{Workflow: *workflow, Verdict: rollup.EfficacyInsufficientData})
			result.Note = telemetryQueryNoRollupNote
			return writeJSONArtifact(result, stdout, stderr)
		}
		if *format == telemetryQueryTutorHoldoutsFormat {
			efficacyThresholds := rollup.DefaultEfficacyThresholds()
			efficacyThresholds.MinSamples = thresholds.MinSamples
			result, verifyErr := verifyTutorHoldouts(
				root,
				queryGaggle,
				nil,
				*window,
				since,
				time.Now().UTC(),
				efficacyThresholds,
			)
			if verifyErr != nil {
				pf(stderr, "error: verify Tutor live holdouts: %v\n", verifyErr)
				return 1
			}
			if !result.NoWork {
				result.Note = telemetryQueryNoRollupNote
			}
			return writeJSONArtifact(result, stdout, stderr)
		}
		result := newCandidateFindingsArtifact(*window, since, nil, telemetryQueryNoRollupNote)
		return writeCandidateFindingsArtifact(result, stdout, stderr)
	}
	db, err := openRollup(l, false)
	if err != nil {
		pf(stderr, "error: open telemetry rollup %s: %v\n", dbPath, err)
		return 1
	}
	defer func() { _ = db.Close() }()

	if *format == telemetryQueryEffectiveVersionEfficacyFormat {
		efficacyThresholds := rollup.DefaultEfficacyThresholds()
		efficacyThresholds.MinSamples = thresholds.MinSamples
		assessment, err := db.AssessLatestEfficacyByEffectiveVersionForGaggle(context.Background(), queryGaggle, *workflow, since, efficacyThresholds)
		if err != nil {
			pf(stderr, "error: assess effective-version efficacy: %v\n", err)
			return 1
		}
		return writeJSONArtifact(newEffectiveVersionEfficacyArtifact(*window, since, *workflow, assessment), stdout, stderr)
	}
	if *format == telemetryQueryTutorHoldoutsFormat {
		efficacyThresholds := rollup.DefaultEfficacyThresholds()
		efficacyThresholds.MinSamples = thresholds.MinSamples
		result, err := verifyTutorHoldouts(
			root,
			queryGaggle,
			db,
			*window,
			since,
			time.Now().UTC(),
			efficacyThresholds,
		)
		if err != nil {
			pf(stderr, "error: verify Tutor live holdouts: %v\n", err)
			return 1
		}
		return writeJSONArtifact(result, stdout, stderr)
	}

	var creditStore *readmodel.Store
	if _, statErr := os.Stat(l.ReadDB()); statErr == nil {
		creditStore, err = readmodel.Open(l.ReadDB())
		if err != nil {
			pf(stderr, "error: open run read model %s: %v\n", l.ReadDB(), err)
			return 1
		}
		defer func() { _ = creditStore.Close() }()
	} else if !os.IsNotExist(statErr) {
		pf(stderr, "error: inspect run read model %s: %v\n", l.ReadDB(), statErr)
		return 1
	}
	result, err := detectCandidateFindingsWithCredit(db, creditStore, *window, since, queryGaggle, aggregates, thresholds)
	if err != nil {
		pf(stderr, "error: query candidate findings: %v\n", err)
		return 1
	}
	return writeCandidateFindingsArtifact(result, stdout, stderr)
}

func resolveTelemetryQueryGaggle(root, workflowName string) (string, error) {
	set, report, err := instance.LoadConfigDir(instance.NewLayout(root).ConfigDir())
	if err != nil {
		return "", fmt.Errorf("load configuration: %w (report: %+v)", err, report)
	}
	var candidates []string
	for _, workflow := range set.Workflows {
		if workflow.Name == workflowName {
			candidates = append(candidates, workflow.Spec.Gaggle)
		}
	}
	sort.Strings(candidates)
	if len(candidates) > 1 {
		return "", fmt.Errorf(
			"workflow %q is ambiguous; candidate gaggles: %s; retry with --gaggle <name>",
			workflowName, strings.Join(candidates, ", "),
		)
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	return "", nil
}

func detectCandidateFindings(
	db *rollup.DB,
	window time.Duration,
	since time.Time,
	gaggle string,
	aggregates telemetryAggregateValues,
	thresholds rollup.Thresholds,
) (candidateFindingsArtifact, error) {
	return detectCandidateFindingsWithCredit(db, nil, window, since, gaggle, aggregates, thresholds)
}

func detectCandidateFindingsWithCredit(
	db *rollup.DB,
	creditStore *readmodel.Store,
	window time.Duration,
	since time.Time,
	gaggle string,
	aggregates telemetryAggregateValues,
	thresholds rollup.Thresholds,
) (candidateFindingsArtifact, error) {
	if thresholds == (rollup.Thresholds{}) {
		thresholds = rollup.DefaultThresholds()
	}
	findings, err := db.Detect(context.Background(), rollup.DetectRequest{
		StatsRequest: rollup.StatsRequest{Gaggle: gaggle, Since: since},
		Thresholds:   thresholds,
	})
	if err != nil {
		return candidateFindingsArtifact{}, err
	}
	if creditStore != nil && (len(aggregates) == 0 || aggregates.includes(rollup.FindingCreditAssignment)) {
		credits, creditErr := creditStore.CreditAssignment(context.Background(), readmodel.CreditOptions{
			Gaggle: gaggle, Since: since,
		})
		if creditErr != nil {
			return candidateFindingsArtifact{}, fmt.Errorf("credit assignment: %w", creditErr)
		}
		for _, credit := range credits {
			if credit.RoutedRuns < thresholds.MinCreditRuns {
				continue
			}
			failureShare := float64(credit.FailureRuns) / float64(credit.RoutedRuns)
			if failureShare < thresholds.MinCreditFailureShare {
				continue
			}
			runIDs, runErr := creditStore.CreditAssignmentRunIDs(context.Background(), readmodel.CreditOptions{
				Gaggle: gaggle, Since: since,
			}, credit, thresholds.MaxFlaggedRuns)
			if runErr != nil {
				return candidateFindingsArtifact{}, fmt.Errorf("credit assignment evidence: %w", runErr)
			}
			flaggedRuns := make([]rollup.JournalPointer, 0, len(runIDs))
			for _, runID := range runIDs {
				flaggedRuns = append(flaggedRuns, rollup.JournalPointer{RunID: runID})
			}
			subject := credit.Workflow + "/" + credit.Kind + "/" + credit.Stage
			if credit.Identity != "" {
				subject += "/" + credit.Identity
			}
			findings = append(findings, rollup.Finding{
				Kind: rollup.FindingCreditAssignment, Subject: subject,
				FlaggedRuns: flaggedRuns,
				Metrics: map[string]float64{
					"routedRuns":         float64(credit.RoutedRuns),
					"failureRuns":        float64(credit.FailureRuns),
					"failureShare":       failureShare,
					"escalationRuns":     float64(credit.EscalationRuns),
					"retryWasteAttempts": float64(credit.RetryWasteAttempts),
				},
				Threshold: thresholds.MinCreditFailureShare,
				NominationGuardrails: &rollup.NominationGuardrails{
					DedupeKey:                  creditAssignmentDedupeKey(credit),
					RequiresUpstreamCauseCheck: true,
					RequiresHumanReview:        creditTargetRequiresHumanReview(credit.Kind),
					GoverningTargetTreatment:   rollup.CreditGoverningTargetTreatment,
				},
			})
		}
	}

	filtered := make([]rollup.Finding, 0, len(findings))
	for _, finding := range findings {
		if !aggregates.includes(finding.Kind) {
			continue
		}
		if finding.FlaggedRuns == nil {
			finding.FlaggedRuns = []rollup.JournalPointer{}
		}
		filtered = append(filtered, finding)
	}
	note := ""
	if len(filtered) == 0 {
		note = telemetryQueryNoFindingsNote
	}
	return newCandidateFindingsArtifact(window, since, filtered, note), nil
}

func creditAssignmentDedupeKey(credit readmodel.NodeCredit) string {
	canonical := strings.Join([]string{
		credit.Gaggle,
		credit.Workflow,
		credit.Kind,
		credit.Stage,
		credit.Identity,
	}, "\x00")
	return fmt.Sprintf("credit-assignment:sha256:%x", sha256.Sum256([]byte(canonical)))
}

func creditTargetRequiresHumanReview(kind string) bool {
	switch strings.ToLower(kind) {
	case "gate", "prompt", "workflow":
		return true
	default:
		return false
	}
}

func newCandidateFindingsArtifact(window time.Duration, since time.Time, findings []rollup.Finding, note string) candidateFindingsArtifact {
	if findings == nil {
		findings = []rollup.Finding{}
	}
	return candidateFindingsArtifact{
		Schema:   candidateFindingsSchemaVersion,
		Window:   window.String(),
		Since:    since,
		Findings: findings,
		NoWork:   len(findings) == 0,
		Note:     note,
	}
}

func writeJSONArtifact(result any, stdout, stderr io.Writer) int {
	return writeJSONArtifactWithSchema(result, "", stdout, stderr)
}

func writeCandidateFindingsArtifact(result candidateFindingsArtifact, stdout, stderr io.Writer) int {
	return writeJSONArtifactWithSchema(result, schemas.CandidateFindings, stdout, stderr)
}

func writeJSONArtifactWithSchema(result any, schemaFile string, stdout, stderr io.Writer) int {
	out, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		pf(stderr, "error: encode telemetry-query artifact: %v\n", err)
		return 1
	}
	if schemaFile != "" {
		if err := validateSchemaJSON(schemaFile, out); err != nil {
			pf(stderr, "error: validate telemetry-query artifact: %v\n", err)
			return 1
		}
	}
	out = append(out, '\n')

	if rf := providerInput(executor.InputResultFile, ""); rf != "" {
		if err := os.WriteFile(rf, out, 0o644); err != nil {
			pf(stderr, "error: write result file %q: %v\n", rf, err)
			return 1
		}
		return 0
	}
	if _, err := stdout.Write(out); err != nil {
		return 2
	}
	return 0
}
