package gagglehealth

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

const (
	defaultDetectorBudget   = 10 * time.Second
	defaultEvaluationBudget = 30 * time.Second
)

// ObservationStatus keeps absence, health, findings, and uncertainty distinct.
type ObservationStatus string

const (
	ObservationNotObserved   ObservationStatus = "not-observed"
	ObservationHealthy       ObservationStatus = "healthy"
	ObservationFinding       ObservationStatus = "finding"
	ObservationIndeterminate ObservationStatus = "indeterminate"
)

// EvidenceDependency identifies one bounded snapshot section a detector needs.
type EvidenceDependency string

const (
	EvidenceWorkflows      EvidenceDependency = "workflows"
	EvidenceRuns           EvidenceDependency = "runs"
	EvidenceClaims         EvidenceDependency = "claims"
	EvidenceRunners        EvidenceDependency = "runners"
	EvidenceReconciliation EvidenceDependency = "reconciliation"
	EvidenceProviders      EvidenceDependency = "providers"
	EvidenceWorkers        EvidenceDependency = "workers"
)

// Snapshot is the read-only, bounded evidence supplied to detectors.
type Snapshot struct {
	Gaggle         string
	CapturedAt     time.Time
	Workflows      []RuntimeSummary
	Runs           []RuntimeSummary
	Claims         []RuntimeSummary
	Runners        []RuntimeSummary
	Reconciliation []RuntimeSummary
	Providers      []RuntimeSummary
	Workers        []RuntimeSummary
}

// RuntimeSummary is a provider-neutral bounded state record.
type RuntimeSummary struct {
	ID         string
	State      string
	UpdatedAt  time.Time
	Attributes map[string]string
}

// SnapshotSource builds snapshots through daemon-owned supported seams.
type SnapshotSource interface {
	Snapshot(context.Context, string, []EvidenceDependency) (Snapshot, error)
}

// Observation is one detector result. Healthy observations identify an
// episode to resolve; findings carry the complete bounded finding snapshot.
type Observation struct {
	Status     ObservationStatus
	EpisodeKey string
	Finding    *apiv1.GaggleHealthFinding
	Detail     string
}

// Detector observes a snapshot and never mutates runtime state directly.
type Detector interface {
	Name() string
	Dependencies() []EvidenceDependency
	Budget() time.Duration
	Evaluate(context.Context, Snapshot, apiv1.GaggleHealthPolicy) ([]Observation, error)
}

// GaggleRegistration atomically replaces one gaggle controller generation.
type GaggleRegistration struct {
	Name      string
	Policy    apiv1.GaggleHealthPolicy
	Detectors []Detector
}

// ControllerOptions bounds controller resource use.
type ControllerOptions struct {
	MaxConcurrent    int
	EvaluationBudget time.Duration
	Clock            func() time.Time
}

// ControllerStatus is the stable daemon read model for controller freshness.
type ControllerStatus struct {
	Gaggle                   string
	FreshAt                  time.Time
	LastSuccessfulEvaluation time.Time
	LastError                string
	NextEvaluation           time.Time
	ActiveFindings           int
	Evaluating               bool
}

// Controller owns daemon-local gaggle evaluation independently of workflows.
type Controller struct {
	store            *Store
	source           SnapshotSource
	now              func() time.Time
	slots            chan struct{}
	detectorSlots    chan struct{}
	evaluationBudget time.Duration

	ctx    context.Context
	cancel context.CancelFunc

	mu         sync.RWMutex
	loops      map[string]*gaggleLoop
	status     map[string]ControllerStatus
	generation uint64
	wg         sync.WaitGroup

	snapshotMu       sync.Mutex
	snapshotsRunning map[string]struct{}
}

type gaggleLoop struct {
	registration GaggleRegistration
	generation   uint64
	wake         chan struct{}
	cancel       context.CancelFunc
}

// NewController constructs a stopped controller.
func NewController(store *Store, source SnapshotSource, options ControllerOptions) (*Controller, error) {
	if store == nil || source == nil {
		return nil, errors.New("gagglehealth: controller store and snapshot source are required")
	}
	if options.MaxConcurrent <= 0 {
		options.MaxConcurrent = 2
	}
	if options.EvaluationBudget <= 0 {
		options.EvaluationBudget = defaultEvaluationBudget
	}
	if options.Clock == nil {
		options.Clock = time.Now
	}
	return &Controller{
		store: store, source: source, now: options.Clock,
		slots:            make(chan struct{}, options.MaxConcurrent),
		detectorSlots:    make(chan struct{}, options.MaxConcurrent),
		evaluationBudget: options.EvaluationBudget,
		loops:            make(map[string]*gaggleLoop), status: make(map[string]ControllerStatus),
		snapshotsRunning: make(map[string]struct{}),
	}, nil
}

// Start begins daemon-owned evaluation and installs the initial generation.
func (c *Controller) Start(ctx context.Context, registrations []GaggleRegistration) error {
	c.mu.Lock()
	if c.cancel != nil {
		c.mu.Unlock()
		return errors.New("gagglehealth: controller already started")
	}
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.mu.Unlock()
	if err := c.Reload(registrations); err != nil {
		c.Stop()
		return err
	}
	return nil
}

// Reload atomically validates and replaces all gaggle controller generations.
func (c *Controller) Reload(registrations []GaggleRegistration) error {
	next := make(map[string]GaggleRegistration, len(registrations))
	for _, registration := range registrations {
		if err := validateRegistration(registration); err != nil {
			return err
		}
		if _, exists := next[registration.Name]; exists {
			return fmt.Errorf("gagglehealth: duplicate gaggle %q", registration.Name)
		}
		if registration.Policy.Enabled != nil && !*registration.Policy.Enabled {
			continue
		}
		next[registration.Name] = cloneRegistration(registration)
	}

	c.mu.Lock()
	if c.cancel == nil {
		c.mu.Unlock()
		return errors.New("gagglehealth: controller is not started")
	}
	old := c.loops
	c.generation++
	c.loops = make(map[string]*gaggleLoop, len(next))
	for name, registration := range next {
		ctx, cancel := context.WithCancel(c.ctx)
		loop := &gaggleLoop{registration: registration, generation: c.generation, wake: make(chan struct{}, 1), cancel: cancel}
		c.loops[name] = loop
		c.wg.Add(1)
		go c.runGaggle(ctx, loop)
	}
	for name, loop := range old {
		loop.cancel()
		if _, retained := next[name]; !retained {
			delete(c.status, name)
		}
	}
	c.mu.Unlock()
	return nil
}

// Wake coalesces any number of relevant transition notifications per gaggle.
func (c *Controller) Wake(gaggle string) {
	c.mu.RLock()
	loop := c.loops[gaggle]
	c.mu.RUnlock()
	if loop == nil {
		return
	}
	select {
	case loop.wake <- struct{}{}:
	default:
	}
}

// WakeAll coalesces a daemon transition that may affect more than one gaggle.
func (c *Controller) WakeAll() {
	c.mu.RLock()
	loops := make([]*gaggleLoop, 0, len(c.loops))
	for _, loop := range c.loops {
		loops = append(loops, loop)
	}
	c.mu.RUnlock()
	for _, loop := range loops {
		select {
		case loop.wake <- struct{}{}:
		default:
		}
	}
}

// Status returns a copy of one gaggle's queryable controller state.
func (c *Controller) Status(gaggle string) (ControllerStatus, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	status, ok := c.status[gaggle]
	return status, ok
}

// Stop cancels and drains every controller generation.
func (c *Controller) Stop() {
	c.mu.Lock()
	if c.cancel == nil {
		c.mu.Unlock()
		return
	}
	c.cancel()
	c.cancel = nil
	c.mu.Unlock()
	c.wg.Wait()
}

func (c *Controller) runGaggle(ctx context.Context, loop *gaggleLoop) {
	defer c.wg.Done()
	interval, _ := time.ParseDuration(loop.registration.Policy.EvaluationInterval)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-loop.wake:
		}
		c.evaluate(ctx, loop)
		timer.Reset(interval)
	}
}

func (c *Controller) evaluate(ctx context.Context, loop *gaggleLoop) {
	registration := loop.registration
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		return
	}
	evaluationCtx, cancel := context.WithTimeout(ctx, c.evaluationBudget)
	defer cancel()
	now := c.now().UTC()
	if !c.updateStatusIfCurrent(registration.Name, loop.generation, func(status *ControllerStatus) {
		status.Gaggle = registration.Name
		status.Evaluating = true
		status.FreshAt = now
	}) {
		return
	}
	dependencies := detectorDependencies(registration.Detectors)
	snapshot, err := c.snapshot(evaluationCtx, registration.Name, dependencies)
	if err != nil {
		evaluationErr := fmt.Errorf("snapshot: %w", err)
		observation := c.indeterminateObservation(registration.Name, "snapshot", now, evaluationErr)
		c.finalizeIfCurrent(registration, loop.generation, now, evaluationErr, []Observation{observation})
		return
	}
	snapshot.Gaggle = registration.Name
	snapshot.CapturedAt = now

	observations := []Observation{c.controllerHealthyObservation(registration.Name, "snapshot")}
	var evaluationErrors []error
	for _, detector := range registration.Detectors {
		detectorCtx, cancel := context.WithTimeout(evaluationCtx, detectorBudget(detector))
		type result struct {
			observations []Observation
			err          error
		}
		resultCh := make(chan result, 1)
		select {
		case c.detectorSlots <- struct{}{}:
		default:
			evaluationErrors = append(evaluationErrors, fmt.Errorf("%s: detector execution capacity exhausted", detector.Name()))
			observations = append(observations, c.indeterminateObservation(registration.Name, detector.Name(), now, errors.New("detector execution capacity exhausted")))
			cancel()
			continue
		}
		go func() {
			defer func() { <-c.detectorSlots }()
			results, detectorErr := detector.Evaluate(detectorCtx, snapshot, registration.Policy)
			resultCh <- result{observations: results, err: detectorErr}
		}()
		var results []Observation
		var detectorErr error
		select {
		case result := <-resultCh:
			results, detectorErr = result.observations, result.err
		case <-detectorCtx.Done():
			detectorErr = detectorCtx.Err()
		}
		cancel()
		if detectorErr != nil {
			evaluationErrors = append(evaluationErrors, fmt.Errorf("%s: %w", detector.Name(), detectorErr))
			observations = append(observations, c.indeterminateObservation(registration.Name, detector.Name(), now, detectorErr))
			continue
		}
		indeterminate := false
		for _, observation := range results {
			if err := validateObservation(observation); err != nil {
				evaluationErrors = append(evaluationErrors, fmt.Errorf("%s: %w", detector.Name(), err))
				observations = append(observations, c.indeterminateObservation(registration.Name, detector.Name(), now, err))
				indeterminate = true
				continue
			}
			if observation.Status == ObservationIndeterminate {
				observation = c.indeterminateObservation(registration.Name, detector.Name(), now, errors.New(observation.Detail))
				indeterminate = true
			}
			observations = append(observations, observation)
		}
		if !indeterminate {
			observations = append(observations, c.controllerHealthyObservation(registration.Name, detector.Name()))
		}
	}
	c.finalizeIfCurrent(registration, loop.generation, now, errors.Join(evaluationErrors...), observations)
}

func (c *Controller) snapshot(ctx context.Context, gaggle string, dependencies []EvidenceDependency) (Snapshot, error) {
	c.snapshotMu.Lock()
	if _, running := c.snapshotsRunning[gaggle]; running {
		c.snapshotMu.Unlock()
		return Snapshot{}, errors.New("snapshot acquisition already in progress")
	}
	c.snapshotsRunning[gaggle] = struct{}{}
	c.snapshotMu.Unlock()

	type result struct {
		snapshot Snapshot
		err      error
	}
	resultCh := make(chan result, 1)
	go func() {
		snapshot, err := c.source.Snapshot(ctx, gaggle, dependencies)
		c.snapshotMu.Lock()
		delete(c.snapshotsRunning, gaggle)
		c.snapshotMu.Unlock()
		resultCh <- result{snapshot: snapshot, err: err}
	}()

	select {
	case result := <-resultCh:
		return result.snapshot, result.err
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	}
}

func (c *Controller) finalizeIfCurrent(registration GaggleRegistration, generation uint64, at time.Time, evaluationErr error, observations []Observation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	loop := c.loops[registration.Name]
	if loop == nil || loop.generation != generation {
		return
	}
	if err := c.applyObservations(registration.Name, at, observations); err != nil {
		evaluationErr = errors.Join(evaluationErr, err)
	}
	c.finishEvaluationLocked(registration, at, evaluationErr)
}

func (c *Controller) controllerHealthyObservation(gaggle, detector string) Observation {
	key, _ := EpisodeKey(FindingControllerDegraded, apiv1.GaggleHealthIdentity{Gaggle: gaggle, Worker: detector})
	return Observation{Status: ObservationHealthy, EpisodeKey: key, Detail: "detector evaluation recovered"}
}

func (c *Controller) indeterminateObservation(gaggle, detector string, at time.Time, cause error) Observation {
	identity := apiv1.GaggleHealthIdentity{Gaggle: gaggle, Worker: detector}
	key, _ := EpisodeKey(FindingControllerDegraded, identity)
	detail := boundedDetail(cause.Error())
	return Observation{
		Status: ObservationFinding,
		Finding: &apiv1.GaggleHealthFinding{
			SchemaVersion:      apiv1.GaggleHealthSchemaVersion,
			Code:               FindingControllerDegraded,
			Severity:           apiv1.GaggleHealthSeverityError,
			Contribution:       apiv1.GaggleHealthDegraded,
			Identity:           identity,
			FirstObserved:      at,
			LastObserved:       at,
			ObservationCount:   1,
			EpisodeKey:         key,
			Evidence:           []apiv1.GaggleHealthEvidence{{Kind: "controller-error", Detail: detail}},
			Summary:            "Health evidence could not be evaluated",
			Confidence:         1,
			EvidenceAssessment: detail,
			Repair: apiv1.GaggleHealthRepair{
				RecommendedAction: "inspect the detector and retry evaluation",
				Disposition:       apiv1.GaggleHealthRepairNotAttempted,
				FollowUp:          apiv1.GaggleHealthFollowUpNone,
			},
		},
	}
}

func (c *Controller) applyObservations(gaggle string, now time.Time, observations []Observation) error {
	current, err := c.store.Snapshot(gaggle)
	if err != nil {
		return err
	}
	active := make(map[string]apiv1.GaggleHealthFinding, len(current.Active))
	for _, finding := range current.Active {
		active[finding.EpisodeKey] = finding
	}
	for _, observation := range observations {
		switch observation.Status {
		case ObservationNotObserved, ObservationIndeterminate:
			continue
		case ObservationHealthy:
			existing, ok := active[observation.EpisodeKey]
			if !ok {
				continue
			}
			existing.ResolvedAt = &now
			existing.ResolutionEvidence = []apiv1.GaggleHealthEvidence{{Kind: "controller-evaluation", Detail: boundedDetail(observation.Detail)}}
			existing.Repair.FollowUp = apiv1.GaggleHealthFollowUpResolved
			if _, err := c.store.AppendNext(transition(now, apiv1.GaggleHealthFindingResolved, existing)); err != nil {
				return err
			}
			delete(active, observation.EpisodeKey)
		case ObservationFinding:
			finding := *observation.Finding
			finding.Identity.Gaggle = gaggle
			finding.LastObserved = now
			eventType := apiv1.GaggleHealthFindingOpened
			if existing, ok := active[finding.EpisodeKey]; ok {
				finding.FirstObserved = existing.FirstObserved
				finding.ObservationCount = existing.ObservationCount + 1
				eventType = apiv1.GaggleHealthFindingUpdated
			} else {
				finding.FirstObserved = now
				finding.ObservationCount = 1
			}
			if _, err := c.store.AppendNext(transition(now, eventType, finding)); err != nil {
				return err
			}
			active[finding.EpisodeKey] = finding
		}
	}
	return nil
}

func (c *Controller) finishEvaluationLocked(registration GaggleRegistration, at time.Time, evaluationErr error) {
	_, appendErr := c.store.AppendNext(apiv1.GaggleHealthEvent{
		SchemaVersion: apiv1.GaggleHealthSchemaVersion,
		OccurredAt:    at, Type: apiv1.GaggleHealthEvaluated, Gaggle: registration.Name,
	})
	if appendErr != nil {
		evaluationErr = errors.Join(evaluationErr, appendErr)
	}
	snapshot, snapshotErr := c.store.Snapshot(registration.Name)
	if snapshotErr != nil {
		evaluationErr = errors.Join(evaluationErr, snapshotErr)
	}
	interval, _ := time.ParseDuration(registration.Policy.EvaluationInterval)
	status := c.status[registration.Name]
	status.Evaluating = false
	status.FreshAt = at
	status.NextEvaluation = at.Add(interval)
	status.ActiveFindings = len(snapshot.Active)
	if evaluationErr == nil {
		status.LastSuccessfulEvaluation = at
		status.LastError = ""
	} else {
		status.LastError = boundedDetail(evaluationErr.Error())
	}
	c.status[registration.Name] = status
}

func (c *Controller) updateStatusIfCurrent(gaggle string, generation uint64, update func(*ControllerStatus)) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	loop := c.loops[gaggle]
	if loop == nil || loop.generation != generation {
		return false
	}
	status := c.status[gaggle]
	update(&status)
	c.status[gaggle] = status
	return true
}

func validateRegistration(registration GaggleRegistration) error {
	if err := validateStoreGaggle(registration.Name); err != nil {
		return err
	}
	if err := ValidatePolicy(registration.Policy); err != nil {
		return fmt.Errorf("gagglehealth: policy for %q: %w", registration.Name, err)
	}
	names := map[string]struct{}{}
	for _, detector := range registration.Detectors {
		if detector == nil || detector.Name() == "" {
			return fmt.Errorf("gagglehealth: gaggle %q has unnamed detector", registration.Name)
		}
		if _, exists := names[detector.Name()]; exists {
			return fmt.Errorf("gagglehealth: gaggle %q has duplicate detector %q", registration.Name, detector.Name())
		}
		names[detector.Name()] = struct{}{}
	}
	return nil
}

func validateObservation(observation Observation) error {
	switch observation.Status {
	case ObservationNotObserved, ObservationIndeterminate:
		if observation.Finding != nil {
			return errors.New("non-finding observation carries a finding")
		}
	case ObservationHealthy:
		if observation.EpisodeKey == "" || observation.Finding != nil {
			return errors.New("healthy observation requires only an episode key")
		}
	case ObservationFinding:
		if observation.Finding == nil || observation.Finding.EpisodeKey == "" {
			return errors.New("finding observation requires a keyed finding")
		}
	default:
		return fmt.Errorf("unknown observation status %q", observation.Status)
	}
	return nil
}

func cloneRegistration(registration GaggleRegistration) GaggleRegistration {
	registration.Detectors = append([]Detector(nil), registration.Detectors...)
	return registration
}

func detectorDependencies(detectors []Detector) []EvidenceDependency {
	set := map[EvidenceDependency]struct{}{}
	for _, detector := range detectors {
		for _, dependency := range detector.Dependencies() {
			set[dependency] = struct{}{}
		}
	}
	result := make([]EvidenceDependency, 0, len(set))
	for dependency := range set {
		result = append(result, dependency)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func detectorBudget(detector Detector) time.Duration {
	if budget := detector.Budget(); budget > 0 {
		return budget
	}
	return defaultDetectorBudget
}

func transition(at time.Time, eventType apiv1.GaggleHealthEventType, finding apiv1.GaggleHealthFinding) apiv1.GaggleHealthEvent {
	copy := finding
	return apiv1.GaggleHealthEvent{
		SchemaVersion: apiv1.GaggleHealthSchemaVersion,
		OccurredAt:    at, Type: eventType, Gaggle: finding.Identity.Gaggle,
		EpisodeKey: finding.EpisodeKey, Finding: &copy,
	}
}

func boundedDetail(detail string) string {
	runes := []rune(detail)
	if len(runes) <= MaxEvidenceDetailLength {
		return detail
	}
	return string(runes[:MaxEvidenceDetailLength])
}
