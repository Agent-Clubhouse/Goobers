package main

import (
	"context"
	"crypto/rand"
	"runtime"
	"time"

	"golang.org/x/mod/semver"

	"github.com/goobers/goobers/internal/diagnostics/fleetstate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/prqueue"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/version"
)

const fleetInventoryLimit = 100

type fleetHealthReader interface {
	Gaggles(context.Context, readservice.PageRequest) (readservice.GagglePage, error)
	Workflows(context.Context, string, readservice.PageRequest) (readservice.WorkflowPage, error)
	ListStatusRuns(context.Context, readservice.StatusRunOptions) ([]readservice.RunSummary, error)
	QueueEligibility(context.Context, string, string) (readservice.QueueEligibilityView, error)
}
type fleetHealthSample func(context.Context, time.Time) []telemetry.DiagnosticRecord

type fleetHealthObserver struct {
	reader             fleetHealthReader
	config             *instance.DiagnosticsConfig
	instanceID, bootID string
	startedAt          time.Time
	sequence           int64
	eligibleSince      map[string]time.Time
	scheduler          *readservice.SchedulerStatus
	ready              func() bool
}

func newFleetHealthSampler(root string, identity *daemonIdentity, config *instance.DiagnosticsConfig, reader fleetHealthReader, readiness ...func() bool) fleetHealthSample {
	id, _ := instance.ReadRootIdentity(root)
	started := time.Now().UTC()
	if identity != nil && !identity.StartedAt.IsZero() {
		started = identity.StartedAt.UTC()
	}
	observer := &fleetHealthObserver{reader: reader, config: config, instanceID: id, bootID: rand.Text(), startedAt: started, eligibleSince: make(map[string]time.Time)}
	if len(readiness) > 0 {
		observer.ready = readiness[0]
	}
	return observer.sample
}

func (o *fleetHealthObserver) sample(ctx context.Context, now time.Time) []telemetry.DiagnosticRecord {
	o.sequence++
	if source, ok := o.reader.(interface{ FleetDiagnosticsAvailable(context.Context) bool }); ok && !source.FleetDiagnosticsAvailable(ctx) {
		o.eligibleSince = make(map[string]time.Time)
		attrs := o.identity("", now)
		attrs["state"], attrs["reasonCode"], attrs["windowCoverage"] = "unknown", "observation_unavailable", "unknown"
		return []telemetry.DiagnosticRecord{{Time: now, Name: "goobers.fleet.heartbeat", Attributes: attrs}}
	}
	o.scheduler = nil
	if reader, ok := o.reader.(fleetSchedulerReader); ok {
		if status, err := reader.SchedulerStatus(ctx); err == nil {
			o.scheduler = &status
		}
	}
	page, err := o.reader.Gaggles(ctx, readservice.PageRequest{Limit: fleetInventoryLimit})
	attrs := o.identity("", now)
	attrs["state"], attrs["reasonCode"], attrs["windowCoverage"] = "unknown", "observation_incomplete", "partial"
	if err == nil && !page.Page.HasMore && fleetReadComplete(page.ReadStateEnvelope) {
		attrs["windowCoverage"] = "complete"
		attrs["reasonCode"] = "progress_unconfirmed"
	}
	observeFleetCleanup(attrs, o.scheduler, now)
	if o.ready != nil && !o.ready() {
		attrs["state"], attrs["reasonCode"] = "waiting", "startup"
	}
	records := []telemetry.DiagnosticRecord{{Time: now, Name: "goobers.fleet.heartbeat", Attributes: attrs}}
	if err != nil {
		o.eligibleSince = make(map[string]time.Time)
		return records
	}
	retained := make(map[string]time.Time)
	for _, gaggle := range page.Items {
		if len(records) > fleetInventoryLimit || ctx.Err() != nil {
			break
		}
		record := o.gaggle(ctx, gaggle, now)
		records = append(records, record)
		if since, ok := o.eligibleSince[gaggle.Name]; ok {
			retained[gaggle.Name] = since
		}
	}
	o.eligibleSince = retained
	return records
}

func (o *fleetHealthObserver) identity(gaggle string, now time.Time) map[string]any {
	attrs := map[string]any{
		"schemaVersion": 1, "deploymentId": o.instanceID, "instanceId": o.instanceID,
		"gaggleId": gaggle, "component": "daemon", "bootId": o.bootID,
		"bootStartedAt": o.startedAt.Format(time.RFC3339Nano), "sequence": o.sequence,
		"observedAt": now.UTC().Format(time.RFC3339Nano), "windowStart": o.startedAt.Format(time.RFC3339Nano),
		"version": version.Get().Version, "channel": fleetVersionChannel(version.Get().Version), "buildCommit": version.Get().Commit, "platform": runtime.GOOS + "/" + runtime.GOARCH,
	}
	if o.config != nil {
		attrs["organization"], attrs["environment"], attrs["ownerRef"] = o.config.Organization, o.config.Environment, o.config.OwnerRef
		if owner := o.config.GaggleOwners[gaggle]; owner != "" {
			attrs["ownerRef"] = owner
		}
	}
	return attrs
}

func (o *fleetHealthObserver) gaggle(ctx context.Context, gaggle readservice.Gaggle, now time.Time) telemetry.DiagnosticRecord {
	attrs := o.identity(gaggle.Name, now)
	observation := fleetstate.Observation{ObservedAt: now, Paused: !gaggle.Enabled, Complete: true}
	runs, err := o.reader.ListStatusRuns(ctx, readservice.StatusRunOptions{Gaggle: gaggle.Name, Limit: fleetInventoryLimit})
	if err != nil || len(runs) >= fleetInventoryLimit {
		observation.Complete = false
	}
	if err == nil {
		observeFleetRuns(&observation, runs, attrs, o.startedAt, gaggle.Name)
	}
	o.observeEligibility(ctx, gaggle.Name, now, &observation)
	observeFleetScheduler(&observation, o.scheduler, gaggle.Name, now)
	verdict := fleetstate.Classify(observation, now, o.config.ProgressPeriod(), 2*o.config.HeartbeatPeriod())
	if o.ready != nil && !o.ready() {
		verdict = fleetstate.Verdict{State: "waiting", ReasonCode: "startup"}
	}
	if !fleetProgressWindowContinues(verdict) {
		delete(o.eligibleSince, gaggle.Name)
		observation.OldestEligibleAt = time.Time{}
	}
	attrs["state"], attrs["reasonCode"] = verdict.State, verdict.ReasonCode
	attrs["windowCoverage"] = "partial"
	if observation.Complete && observation.EligibleCount != nil {
		attrs["windowCoverage"] = "complete"
	}
	if observation.EligibleCount != nil {
		attrs["eligibleCount"] = *observation.EligibleCount
	}
	if !observation.OldestEligibleAt.IsZero() {
		attrs["oldestEligibleAt"] = observation.OldestEligibleAt.Format(time.RFC3339Nano)
	}
	if !observation.LastUsefulProgressAt.IsZero() && !observation.LastUsefulProgressAt.After(now) {
		attrs["lastUsefulProgressAt"] = observation.LastUsefulProgressAt.Format(time.RFC3339Nano)
	}
	return telemetry.DiagnosticRecord{Time: now, Name: "goobers.fleet.heartbeat", Attributes: attrs}
}

func observeFleetRuns(o *fleetstate.Observation, runs []readservice.RunSummary, attrs map[string]any, windowStart time.Time, gaggle string) {
	inflight, noWork, retries := 0, 0, 0
	for _, run := range runs {
		if run.Gaggle != gaggle {
			o.Complete = false
			continue
		}
		if !run.Terminal {
			inflight++
		}
		if !run.StartedAt.Before(windowStart) {
			if run.NoWork {
				noWork++
			}
			retries += run.RetryCount
		}
		var progress time.Time
		if run.FinishedAt != nil && !run.NoWork && run.Phase == journal.PhaseCompleted {
			progress = *run.FinishedAt
		}
		if progress.After(o.LastUsefulProgressAt) {
			o.LastUsefulProgressAt = progress
		}
	}
	if o.Complete {
		o.InflightCount, o.NoWorkCount = &inflight, &noWork
	}
	// Partial positive counts are observed lower bounds. Zero is exported only
	// when every relevant run in the window was observed.
	for key, count := range map[string]int{"inflightCount": inflight, "noWorkCount": noWork, "retryCount": retries} {
		if o.Complete || count > 0 {
			attrs[key] = count
		}
	}
}

func (o *fleetHealthObserver) observeEligibility(ctx context.Context, gaggle string, now time.Time, observation *fleetstate.Observation) {
	defer func() {
		if observation.EligibleCount == nil {
			delete(o.eligibleSince, gaggle)
		}
	}()
	workflows, err := o.reader.Workflows(ctx, gaggle, readservice.PageRequest{Limit: fleetInventoryLimit})
	if err != nil || workflows.Page.HasMore || !fleetReadComplete(workflows.ReadStateEnvelope) {
		observation.Complete = false
		return
	}
	count := 0
	for _, wf := range workflows.Items {
		if !wf.Enabled {
			continue
		}
		evidence, err := o.reader.QueueEligibility(ctx, gaggle, wf.Identity.Name)
		if err != nil || evidence.Report == nil || !evidence.Report.CompleteSnapshot || evidence.Report.OmittedItems > 0 || !fleetReadComplete(evidence.ReadStateEnvelope) {
			return
		}
		age := now.Sub(evidence.Report.ObservedAt)
		if age < 0 || age > 2*o.config.HeartbeatPeriod() {
			return
		}
		available, known := fleetAvailableCount(evidence.Report.Items)
		if !known {
			return
		}
		count += available
	}
	observation.EligibleCount = &count
	if count == 0 {
		delete(o.eligibleSince, gaggle)
		return
	}
	since, ok := o.eligibleSince[gaggle]
	if !ok {
		since = now
		o.eligibleSince[gaggle] = since
	}
	observation.OldestEligibleAt = since
}

func fleetReadComplete(envelope readservice.ReadStateEnvelope) bool {
	state := envelope.ReadState
	return state != nil && state.Completeness == readmodel.CompletenessComplete && state.LagSeconds >= 0 && state.LagSeconds <= 30 && state.PendingIntake == 0 && state.IntakeWriteFailures == 0
}

// Selection eligibility alone does not prove availability: another run or
// deployment may hold the item. Unknown claim evidence stays unknown.
func fleetAvailableCount(items []prqueue.Item) (int, bool) {
	count := 0
	for _, item := range items {
		if !item.Eligible {
			continue
		}
		switch item.Claim.State {
		case "held-by-this-run", "held-by-other-run":
			continue
		case "unclaimed", "expired":
			if item.Claim.ProviderClaimLabel {
				return 0, false
			}
			count++
		default:
			return 0, false
		}
	}
	return count, true
}

func fleetVersionChannel(version string) string {
	if !semver.IsValid(version) {
		return "unknown"
	}
	if semver.Prerelease(version) != "" {
		return "preview"
	}
	return "stable"
}

func fleetProgressWindowContinues(verdict fleetstate.Verdict) bool {
	return verdict.State == "stalled" || verdict.State == "productive" || verdict.State == "waiting" && verdict.ReasonCode == "eligible_within_threshold"
}
