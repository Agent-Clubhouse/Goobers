package main

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/instancefixture"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

// The multi-gaggle tenant-load experiment.
//
// The mixed-load experiment (§16.3) answers "does a write load slow reads
// down". It cannot answer "does *another tenant* slow this tenant down",
// because it drives one gaggle: every read it issues is scoped to gaggle 0 and
// every write it applies lands in that gaggle's run root. Multi-tenant cost was
// therefore only ever inferred from journal-history volume, which is a
// different variable — a single gaggle with ten times the history is not ten
// gaggles.
//
// This scenario varies the number of *concurrently active* gaggles and measures
// each level separately. Every tenant reads and writes through the paths they
// actually share — one telemetry.db (and its single writer connection), one
// instance journal behind a cross-process lock, one read service — while
// touching its own run root, so a difference between levels is contention on
// the shared paths rather than a bigger corpus.
//
// Levels are reported side by side rather than reduced to a verdict. Whether a
// given degradation is acceptable is a target question (§14.12), and it is
// settled against a 1x/10x corpus; what the harness owes is comparable numbers
// per tenant count, and the honest statement that a level clamped to the
// generated gaggle count is the level that was actually run.

// TenantLoadSpec parameterizes the multi-tenant scenario. The per-tenant write
// rates are per tenant on purpose: adding a tenant must add its load, or the
// levels differ only in how the same fixed load is spread and the dimension
// measures nothing.
type TenantLoadSpec struct {
	// Levels are the tenant counts to measure, each one a separate phase.
	Levels []int
	// Duration is how long each level is sustained.
	Duration time.Duration
	// ReadersPerTenant is how many concurrent reader goroutines each tenant runs.
	ReadersPerTenant int
	// SchedulerAppendsPerSecPerTenant is each tenant's rate of appends to the
	// shared instance journal — the cross-process-locked path every gaggle's
	// scheduler decisions serialize through.
	SchedulerAppendsPerSecPerTenant int
	// RunAppendsPerSecPerTenant is each tenant's rate of appends to its own run
	// journals, in its own gaggle's run root.
	RunAppendsPerSecPerTenant int
	// IngestPerSecPerTenant is each tenant's rate of rollup ingests, which all
	// tenants apply through the same store connection.
	IngestPerSecPerTenant int
}

// DefaultTenantLoadSpec is the default two-level scenario: one active tenant,
// then four. Four is the generator's baseline gaggle count, so the high level is
// measurable against a default corpus without regenerating one.
func DefaultTenantLoadSpec(d time.Duration) TenantLoadSpec {
	return TenantLoadSpec{
		Levels:                          []int{1, baselineGaggles},
		Duration:                        d,
		ReadersPerTenant:                2,
		SchedulerAppendsPerSecPerTenant: 2,
		RunAppendsPerSecPerTenant:       5,
		IngestPerSecPerTenant:           1,
	}
}

// TenantLoadResult reports every measured level plus how the store-bound read
// moved between the lowest and highest level.
type TenantLoadResult struct {
	Spec   TenantLoadSpecReport `json:"spec"`
	Levels []TenantLevelResult  `json:"levels"`
	// ContentionOperation is the read whose cross-level factor is reported, and
	// ContentionFactor is its p99 at the highest level over its p99 at the
	// lowest. As in the mixed-load experiment this keys on the store-bound read:
	// the scan-bound surfaces are warmed by the other tenants' writes and their
	// factor answers a question they cannot be asked.
	ContentionOperation string  `json:"contentionOperation"`
	ContentionFactor    float64 `json:"contentionFactorP99"`
}

// TenantLoadSpecReport is the spec as reported, with the duration in a readable
// form.
type TenantLoadSpecReport struct {
	DurationSeconds                 float64 `json:"durationSecondsPerLevel"`
	Levels                          []int   `json:"levels"`
	ReadersPerTenant                int     `json:"readersPerTenant"`
	SchedulerAppendsPerSecPerTenant int     `json:"schedulerAppendsPerSecPerTenant"`
	RunAppendsPerSecPerTenant       int     `json:"runAppendsPerSecPerTenant"`
	IngestPerSecPerTenant           int     `json:"ingestPerSecPerTenant"`
}

// TenantLevelResult is one tenant count's measurement.
type TenantLevelResult struct {
	// Tenants is how many gaggles were actually driven, which is not always what
	// was requested: a level above the generated gaggle count is clamped, and a
	// clamped level that reported the requested number would claim a
	// concurrency the corpus cannot express.
	Tenants int `json:"tenants"`
	// RequestedTenants is the level as asked for, so a clamp is visible rather
	// than silent.
	RequestedTenants int             `json:"requestedTenants"`
	Stats            map[string]Stat `json:"stats"`
	WritesApplied    WriteCounts     `json:"writesApplied"`
	Errors           map[string]int  `json:"errors,omitempty"`
}

// tenantStoreBoundOp is the read whose cost is dominated by the shared store
// rather than by a filesystem scan — the gaggle-scoped list page each tenant
// issues for its own gaggle. See storeBoundOp for why the distinction decides
// what may be concluded.
const tenantStoreBoundOp = opListRunsGaggle

// tenantReadMix is one tenant's read pattern: the portal's mix as its own
// gaggle sees it.
//
// It differs from readMix in the list page it issues — gaggle-scoped rather
// than instance-wide — because a tenant reads its own scope, and because a mix
// where every tenant asked the same unscoped question would measure the same
// cached answer N times instead of N tenants contending. The unscoped page is
// kept alongside it so a level stays comparable to the mixed-load experiment's
// numbers for the same operation.
func tenantReadMix(service *readservice.Local, gaggle string) []readOp {
	ctx := context.Background()
	mix := []readOp{
		{opListRunsGaggle, func() error {
			_, err := service.ListRuns(ctx, readservice.RunListOptions{Gaggle: gaggle, Limit: 50})
			return err
		}},
	}
	return append(mix, readMix(service, gaggle)...)
}

// runTenantLoad measures the read mix at each requested tenant level.
func runTenantLoad(
	layout instance.Layout,
	gen GenerateResult,
	spec TenantLoadSpec,
	samples int,
) (TenantLoadResult, error) {
	available := len(gen.Gaggles)
	if available == 0 {
		return TenantLoadResult{}, fmt.Errorf("scale: tenant load needs at least one generated gaggle")
	}
	levels := normalizeTenantLevels(spec.Levels, available)
	if len(levels) < 2 {
		return TenantLoadResult{}, fmt.Errorf(
			"scale: tenant load needs at least two distinct levels within the %d generated gaggles; got %v",
			available, spec.Levels)
	}

	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		return TenantLoadResult{}, fmt.Errorf("scale: open rollup: %w", err)
	}
	defer func() { _ = db.Close() }()

	service, err := readservice.NewLocal(readservice.LocalSources{
		Layout:      layout,
		Definitions: instancefixture.Inventory(gen.Inventory),
		Telemetry:   db,
	}, func() bool { return true })
	if err != nil {
		return TenantLoadResult{}, fmt.Errorf("scale: construct read service: %w", err)
	}

	// The daemon serves the active-run count from a background sample, so the
	// inventory surfaces are only measurable on the path production takes once
	// that sampler has published. Without it every tenant's Instance() read
	// returns "not sampled yet" and the level measures an error rate.
	stopSampler := service.StartActiveRunSampler(time.Hour)
	defer func() { _ = stopSampler() }()
	if err := waitForActiveSample(context.Background(), service); err != nil {
		return TenantLoadResult{}, err
	}

	result := TenantLoadResult{
		Spec: TenantLoadSpecReport{
			DurationSeconds:                 spec.Duration.Seconds(),
			Levels:                          effectiveLevels(levels),
			ReadersPerTenant:                spec.ReadersPerTenant,
			SchedulerAppendsPerSecPerTenant: spec.SchedulerAppendsPerSecPerTenant,
			RunAppendsPerSecPerTenant:       spec.RunAppendsPerSecPerTenant,
			IngestPerSecPerTenant:           spec.IngestPerSecPerTenant,
		},
		ContentionOperation: tenantStoreBoundOp,
	}

	for _, plan := range levels {
		level := measureTenantLevel(layout, gen, service, spec, plan, samples)
		result.Levels = append(result.Levels, level)
	}
	result.ContentionFactor = tenantContentionFactor(result.Levels, tenantStoreBoundOp)
	return result, nil
}

// effectiveLevels is the tenant counts actually driven, in report order.
func effectiveLevels(plans []tenantLevelPlan) []int {
	out := make([]int, 0, len(plans))
	for _, p := range plans {
		out = append(out, p.effective)
	}
	return out
}

// tenantLevelPlan is one level to measure: what was asked for, and what the
// generated corpus can actually express.
type tenantLevelPlan struct {
	requested int
	effective int
}

// normalizeTenantLevels clamps every requested level to the gaggles that were
// actually generated, drops levels that collapse onto one already planned, and
// sorts ascending so the report reads low to high and the cross-level factor has
// a defined direction.
func normalizeTenantLevels(requested []int, available int) []tenantLevelPlan {
	seen := map[int]int{}
	out := make([]tenantLevelPlan, 0, len(requested))
	for _, n := range requested {
		if n < 1 {
			continue
		}
		effective := n
		if effective > available {
			effective = available
		}
		if at, ok := seen[effective]; ok {
			// Two requested levels can collapse onto one effective count (8 and 4
			// both clamp to 4). Keep the one that was asked for exactly, so the
			// report does not describe a level the corpus could express as though
			// it had been clamped.
			if n == effective {
				out[at].requested = n
			}
			continue
		}
		seen[effective] = len(out)
		out = append(out, tenantLevelPlan{requested: n, effective: effective})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].effective < out[j].effective })
	return out
}

// measureTenantLevel drives `tenants` gaggles concurrently for the spec's
// duration and summarizes the pooled read latencies.
//
// Writers start first and are given the same warm-up the mixed-load experiment
// uses, so reads are sampled against a load already at its target rate rather
// than against its ramp.
func measureTenantLevel(
	layout instance.Layout,
	gen GenerateResult,
	service *readservice.Local,
	spec TenantLoadSpec,
	plan tenantLevelPlan,
	samples int,
) TenantLevelResult {
	tenants := plan.effective
	counts := &writeCounters{}
	stop := make(chan struct{})
	var writers sync.WaitGroup
	for t := 0; t < tenants; t++ {
		startTenantWriters(&writers, stop, counts, layout, gen, spec, t)
	}
	time.Sleep(loadWarmup)

	deadline := time.Now().Add(spec.Duration)
	var readers sync.WaitGroup
	var mu sync.Mutex
	pooled := map[string][]time.Duration{}
	errs := map[string]int{}
	for t := 0; t < tenants; t++ {
		readers.Add(1)
		go func(tenant int) {
			defer readers.Done()
			mix := tenantReadMix(service, gen.Gaggles[tenant])
			for {
				raw, passErrs := sampleMixRaw(mix, samples, spec.ReadersPerTenant)
				mu.Lock()
				for op, ds := range raw {
					pooled[op] = append(pooled[op], ds...)
				}
				mergeErrors(errs, passErrs)
				mu.Unlock()
				if time.Now().After(deadline) {
					return
				}
			}
		}(t)
	}
	readers.Wait()

	close(stop)
	writers.Wait()

	stats := map[string]Stat{}
	for op, ds := range pooled {
		stats[op] = summarize(op, ds)
	}
	return TenantLevelResult{
		Tenants:          tenants,
		RequestedTenants: plan.requested,
		Stats:            stats,
		WritesApplied: WriteCounts{
			SchedulerAppends:            counts.schedulerAppends.Load(),
			RunAppends:                  counts.runAppends.Load(),
			Ingests:                     counts.ingests.Load(),
			SlowestSchedulerAppendNanos: time.Duration(counts.slowestSchedulerAppend.Load()),
		},
		Errors: errs,
	}
}

// startTenantWriters launches one tenant's share of the write load: appends to
// the shared instance journal, appends to that tenant's own run journals, and
// ingests through the shared store. The first and third are the contended
// paths; the second is deliberately gaggle-local, so what the levels differ in
// is sharing rather than a hotter single directory.
func startTenantWriters(
	wg *sync.WaitGroup,
	stop <-chan struct{},
	counts *writeCounters,
	layout instance.Layout,
	gen GenerateResult,
	spec TenantLoadSpec,
	tenant int,
) {
	if spec.SchedulerAppendsPerSecPerTenant > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			schedulerAppendLoad(stop, counts, layout, spec.SchedulerAppendsPerSecPerTenant)
		}()
	}
	runs := tenantRunIDs(gen, tenant, true)
	if spec.RunAppendsPerSecPerTenant > 0 && len(runs) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tickLoad(stop, spec.RunAppendsPerSecPerTenant, func(i int) {
				if appendHeartbeat(layout, runs[i%len(runs)]) == nil {
					counts.runAppends.Add(1)
				}
			})
		}()
	}
	ingestable := tenantRunIDs(gen, tenant, false)
	if spec.IngestPerSecPerTenant > 0 && len(ingestable) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			db, err := rollup.Open(layout.TelemetryDB())
			if err != nil {
				return
			}
			defer func() { _ = db.Close() }()
			tickLoad(stop, spec.IngestPerSecPerTenant, func(i int) {
				dir, err := layout.FindRunDir(ingestable[i%len(ingestable)])
				if err != nil {
					return
				}
				if err := db.IngestRun(context.Background(), dir); err == nil {
					counts.ingests.Add(1)
				}
			})
		}()
	}
}

// tickLoad calls do at perSec until stop closes, passing a monotonically
// increasing index so the caller can walk a work list without keeping its own
// counter.
func tickLoad(stop <-chan struct{}, perSec int, do func(i int)) {
	tick := time.NewTicker(time.Second / time.Duration(perSec))
	defer tick.Stop()
	for i := 0; ; i++ {
		select {
		case <-stop:
			return
		case <-tick.C:
			do(i)
		}
	}
}

// tenantRunIDs returns run ids belonging to one tenant's gaggle.
//
// The generator attributes run `i` to gaggle `i % len(gaggles)` and leaves every
// seventh run in flight, so inFlightOnly selects the runs that can legitimately
// take another event — appending to a finished run would count an error rather
// than a write. The list is capped because a load loop only needs enough runs to
// avoid hammering one directory, and materializing every id at 10x would cost
// megabytes to walk a few hundred entries.
func tenantRunIDs(gen GenerateResult, tenant int, inFlightOnly bool) []string {
	gaggles := len(gen.Gaggles)
	if gaggles == 0 || gen.Runs == 0 || tenant >= gaggles {
		return nil
	}
	ids := make([]string, 0, tenantRunSampleCap)
	for i := tenant; i < gen.Runs && len(ids) < tenantRunSampleCap; i += gaggles {
		if inFlightOnly && i%7 != 0 {
			continue
		}
		ids = append(ids, fmt.Sprintf("run-%08d", i))
	}
	return ids
}

// tenantRunSampleCap bounds the per-tenant run list a writer walks.
const tenantRunSampleCap = 512

// tenantContentionFactor is the named operation's p99 at the highest measured
// level over its p99 at the lowest — the number the whole dimension exists to
// produce. Zero when either level lacks the operation, rather than a fabricated
// 1.0 that would read as "no contention".
func tenantContentionFactor(levels []TenantLevelResult, op string) float64 {
	if len(levels) < 2 {
		return 0
	}
	low, lowOK := levels[0].Stats[op]
	high, highOK := levels[len(levels)-1].Stats[op]
	if !lowOK || !highOK || low.P99 <= 0 {
		return 0
	}
	return float64(high.P99) / float64(low.P99)
}
