package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"runtime"
	"strings"
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

const (
	fleetInventoryPageLimit = 100
	// A single instance is expected to carry only a handful of gaggles and
	// workflows. Keep an explicit ceiling for corrupt or adversarial inventory,
	// while following ordinary read-service cursors instead of silently treating
	// the first page as the whole fleet.
	fleetInventoryObservationLimit = 1000
	fleetRunObservationLimit       = 100
	fleetGaggleSnapshotInterval    = time.Minute
)

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
	workers            *fleetWorkerHealthObserver
	scheduler          *readservice.SchedulerStatus
	ready              func() bool
	lastGaggleEmission map[string]fleetGaggleEmission
}

type fleetGaggleEmission struct {
	signature string
	at        time.Time
}

func newFleetHealthSampler(root string, identity *daemonIdentity, config *instance.DiagnosticsConfig, reader fleetHealthReader, workers *fleetWorkerHealthObserver, readiness ...func() bool) fleetHealthSample {
	id, _ := instance.ReadRootIdentity(root)
	started := time.Now().UTC()
	if identity != nil && !identity.StartedAt.IsZero() {
		started = identity.StartedAt.UTC()
	}
	// Keep this nonsecret identity outside credential-shaped uppercase patterns.
	observer := &fleetHealthObserver{workers: workers, reader: reader, config: config, instanceID: id, bootID: strings.ToLower(rand.Text()), startedAt: started, eligibleSince: make(map[string]time.Time), lastGaggleEmission: make(map[string]fleetGaggleEmission)}
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
	o.workers.beginPulse(ctx, now)
	o.scheduler = nil
	if reader, ok := o.reader.(fleetSchedulerReader); ok {
		if status, err := reader.SchedulerStatus(ctx); err == nil {
			o.scheduler = &status
		}
	}
	gaggles, complete, err := o.gaggles(ctx)
	attrs := o.identity("", now)
	attrs["state"], attrs["reasonCode"], attrs["windowCoverage"] = "unknown", "observation_incomplete", "partial"
	if err == nil && complete {
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
	seen := make(map[string]bool, len(gaggles))
	for _, gaggle := range gaggles {
		if ctx.Err() != nil {
			break
		}
		record := o.gaggle(ctx, gaggle, now)
		seen[gaggle.Name] = true
		if o.shouldEmitGaggle(gaggle.Name, record.Attributes, now) {
			records = append(records, record)
		}
		if since, ok := o.eligibleSince[gaggle.Name]; ok {
			retained[gaggle.Name] = since
		}
	}
	o.eligibleSince = retained
	if complete {
		for gaggle := range o.lastGaggleEmission {
			if !seen[gaggle] {
				delete(o.lastGaggleEmission, gaggle)
			}
		}
	}
	return records
}

func (o *fleetHealthObserver) shouldEmitGaggle(gaggle string, attrs map[string]any, now time.Time) bool {
	if o.lastGaggleEmission == nil {
		o.lastGaggleEmission = make(map[string]fleetGaggleEmission)
	}
	keys := []string{
		"state", "reasonCode", "windowCoverage",
		"workerState", "workerReasonCode", "workerCoverage",
		"backlogState", "backlogReasonCode", "backlogCoverage",
		"requiredMcpState", "requiredMcpReason", "requiredMcpCoverage",
	}
	var signature strings.Builder
	for _, key := range keys {
		_, _ = fmt.Fprintf(&signature, "%s=%v\x00", key, attrs[key])
	}
	previous, ok := o.lastGaggleEmission[gaggle]
	changed := !ok || previous.signature != signature.String()
	periodic := ok && (now.Before(previous.at) || now.Sub(previous.at) >= fleetGaggleSnapshotInterval)
	if !changed && !periodic {
		return false
	}
	o.lastGaggleEmission[gaggle] = fleetGaggleEmission{signature: signature.String(), at: now}
	return true
}

func (o *fleetHealthObserver) gaggles(ctx context.Context) ([]readservice.Gaggle, bool, error) {
	items := make([]readservice.Gaggle, 0, fleetInventoryPageLimit)
	cursor := ""
	complete := true
	for len(items) < fleetInventoryObservationLimit {
		page, err := o.reader.Gaggles(ctx, readservice.PageRequest{Limit: fleetInventoryPageLimit, Cursor: cursor})
		if err != nil {
			return items, false, err
		}
		complete = complete && fleetReadComplete(page.ReadStateEnvelope)
		remaining := fleetInventoryObservationLimit - len(items)
		if len(page.Items) > remaining {
			items = append(items, page.Items[:remaining]...)
			return items, false, nil
		}
		items = append(items, page.Items...)
		if !page.Page.HasMore {
			if page.Page.Total > 0 && len(items) != page.Page.Total {
				complete = false
			}
			return items, complete, nil
		}
		if page.Page.NextCursor == "" || page.Page.NextCursor == cursor {
			return items, false, nil
		}
		cursor = page.Page.NextCursor
	}
	return items, false, nil
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
	runs, err := o.reader.ListStatusRuns(ctx, readservice.StatusRunOptions{Gaggle: gaggle.Name, Limit: fleetRunObservationLimit + 1})
	if err != nil || len(runs) > fleetRunObservationLimit {
		observation.Complete = false
	}
	if err == nil {
		if len(runs) > fleetRunObservationLimit {
			runs = runs[:fleetRunObservationLimit]
		}
		observeFleetRuns(&observation, runs, attrs, o.startedAt, gaggle.Name)
	}
	fleetMCPAttributes(attrs, fleetMCPHealth(runs, gaggle.Name, observation.Complete, now))
	o.observeEligibility(ctx, gaggle.Name, now, &observation)
	if observation.Complete {
		observation.BackoffUntil = fleetRetryBackoff(runs, gaggle.Name, now, o.startedAt)
	}
	observeFleetScheduler(&observation, o.scheduler, gaggle.Name, now)
	o.workers.observe(gaggle.Name, &observation, attrs)
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
	inflight, noWork, retries, waitingGates := 0, 0, 0, 0
	for _, run := range runs {
		if run.Gaggle != gaggle {
			o.Complete = false
			continue
		}
		if !run.Terminal {
			if run.WaitingForGate {
				waitingGates++
			}
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
		o.ActiveDeadline = fleetExecutionDeadline(runs, gaggle, o.ObservedAt)
		o.WaitingOnUser = inflight > 0 && waitingGates == inflight
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
	workflows, complete, err := o.workflows(ctx, gaggle)
	if err != nil || !complete {
		observation.Complete = false
		return
	}
	count := 0
	for _, wf := range workflows {
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

func (o *fleetHealthObserver) workflows(ctx context.Context, gaggle string) ([]readservice.WorkflowSummary, bool, error) {
	items := make([]readservice.WorkflowSummary, 0, fleetInventoryPageLimit)
	cursor := ""
	complete := true
	for len(items) < fleetInventoryObservationLimit {
		page, err := o.reader.Workflows(ctx, gaggle, readservice.PageRequest{Limit: fleetInventoryPageLimit, Cursor: cursor})
		if err != nil {
			return items, false, err
		}
		complete = complete && fleetReadComplete(page.ReadStateEnvelope)
		remaining := fleetInventoryObservationLimit - len(items)
		if len(page.Items) > remaining {
			items = append(items, page.Items[:remaining]...)
			return items, false, nil
		}
		items = append(items, page.Items...)
		if !page.Page.HasMore {
			if page.Page.Total > 0 && len(items) != page.Page.Total {
				complete = false
			}
			return items, complete, nil
		}
		if page.Page.NextCursor == "" || page.Page.NextCursor == cursor {
			return items, false, nil
		}
		cursor = page.Page.NextCursor
	}
	return items, false, nil
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
