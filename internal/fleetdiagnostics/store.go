package fleetdiagnostics

import (
	"errors"
	"sort"
	"sync"
	"time"

	"golang.org/x/mod/semver"
)

const (
	// MaxInventory bounds enrollment and all per-identity observation state.
	MaxInventory = 1024
	// MaxTenants bounds independently authenticated company policies.
	MaxTenants = 64
	// MaxOwners bounds explicit routing references per company.
	MaxOwners = 128
	// MaxTransitions bounds recent condition changes per enrolled identity.
	MaxTransitions = 16
)

// Key is scoped by an authenticated tenant, not by any event attribute.
type Key struct{ DeploymentID, InstanceID, GaggleID string }

// Enrollment is explicit inventory, including deployments never observed.
type Enrollment struct {
	Identity
	HeartbeatInterval time.Duration
	MissedIntervals   int
	Pin               string
	MaintenanceStart  time.Time
	MaintenanceEnd    time.Time
}

// Transition is a bounded history of evaluated state changes, including recovery.
type Transition struct {
	Condition string    `json:"condition,omitempty"`
	At        time.Time `json:"at"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Reason    string    `json:"reason"`
}

type entry struct {
	lastMCPState string
	enrollment   Enrollment
	enrolledAt   time.Time
	heartbeat    *Heartbeat
	receivedAt   time.Time
	features     map[string]FeatureUsage
	transitions  []Transition
	lastState    string
}

// Backend is a bounded in-memory reference receiver. It retains the latest
// heartbeat and one absolute window per finite feature per enrolled identity.
// It is deliberately not a durable production fleet database.
type Backend struct {
	mu           sync.Mutex
	now          func() time.Time
	maxClockSkew time.Duration
	tenants      map[string]Tenant
	entries      map[string]map[Key]*entry
	inventory    int
}

// Tenant contains company-controlled labels, owner routing and release policy.
// Owner routes never cause a send; reports merely return the configured target.
type Tenant struct {
	Organization         string
	Owners               map[string]string
	Catalogue            Catalogue
	CollectorUnavailable bool
}

// New creates a reference backend with a fakeable clock and clock-skew budget.
func New(now func() time.Time, maxClockSkew time.Duration) (*Backend, error) {
	if now == nil || maxClockSkew < 0 || maxClockSkew > time.Hour {
		return nil, errors.New("invalid clock policy")
	}
	return &Backend{now: now, maxClockSkew: maxClockSkew, tenants: map[string]Tenant{}, entries: map[string]map[Key]*entry{}}, nil
}

// SetTenant configures or replaces a bounded company policy. Caller-owned maps
// are copied. It cannot enroll records from another tenant implicitly.
func (b *Backend) SetTenant(id string, policy Tenant) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if id == "" || len(id) > 256 || len(policy.Organization) > 256 || len(policy.Owners) > MaxOwners {
		return errors.New("invalid tenant policy")
	}
	if _, exists := b.tenants[id]; !exists && len(b.tenants) >= MaxTenants {
		return errors.New("tenant limit reached")
	}
	if err := policy.Catalogue.validate(); err != nil {
		return err
	}
	owners := make(map[string]string, len(policy.Owners))
	for k, v := range policy.Owners {
		if k == "" || len(k) > 256 || len(v) > 1024 {
			return errors.New("invalid owner route")
		}
		owners[k] = v
	}
	policy.Owners = owners
	policy.Catalogue = policy.Catalogue.clone()
	b.tenants[id] = policy
	if b.entries[id] == nil {
		b.entries[id] = map[Key]*entry{}
	}
	return nil
}

// Enroll establishes the inventory against which missing heartbeats are judged.
// Re-enrollment updates policy without erasing liveness or deduplication state.
func (b *Backend) Enroll(tenant string, enrollment Enrollment) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	policy, ok := b.tenants[tenant]
	if !ok {
		return errors.New("unknown tenant")
	}
	if enrollment.DeploymentID == "" || enrollment.InstanceID == "" || enrollment.Component == "" || enrollment.HeartbeatInterval < time.Second || enrollment.HeartbeatInterval > time.Hour || enrollment.MissedIntervals < 1 || enrollment.MissedIntervals > 100 {
		return errors.New("invalid enrollment")
	}
	if policy.Organization != enrollment.Organization {
		return errors.New("organization does not match enrollment tenant")
	}
	if err := validEnrollment(enrollment); err != nil {
		return err
	}
	key := identityKey(enrollment.Identity)
	if existing := b.entries[tenant][key]; existing != nil {
		existing.enrollment = enrollment
		return nil
	}
	if b.inventory >= MaxInventory {
		return errors.New("inventory limit reached")
	}
	b.entries[tenant][key] = &entry{enrollment: enrollment, enrolledAt: b.now(), features: map[string]FeatureUsage{}}
	b.inventory++
	return nil
}

// Remove retires inventory and its bounded state; it does not mark a missing
// daemon healthy or implicitly retire it because it stopped reporting.
func (b *Backend) Remove(tenant string, key Key) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.entries[tenant][key] != nil {
		delete(b.entries[tenant], key)
		b.inventory--
	}
}

func identityKey(i Identity) Key { return Key{i.DeploymentID, i.InstanceID, i.GaggleID} }
func (b *Backend) enrolled(tenant string, i Identity) (*entry, error) {
	policy, ok := b.tenants[tenant]
	if !ok {
		return nil, errors.New("unauthorized tenant")
	}
	e := b.entries[tenant][identityKey(i)]
	if e == nil || i.Organization != policy.Organization || i.Environment != e.enrollment.Environment || i.Component != e.enrollment.Component {
		return nil, errors.New("record is not enrolled in authenticated tenant")
	}
	return e, nil
}
func (b *Backend) admissible(w Window, previous *Window) (bool, error) {
	if w.ObservedAt.After(b.now().Add(b.maxClockSkew)) {
		return false, errors.New("observation exceeds clock-skew budget")
	}
	if previous == nil {
		return true, nil
	}
	if w.BootID == previous.BootID {
		if !w.BootStartedAt.Equal(previous.BootStartedAt) {
			return false, errors.New("boot identity changed timestamp")
		}
		if w.Sequence <= previous.Sequence {
			return false, nil
		}
		if w.ObservedAt.Before(previous.ObservedAt) {
			return false, errors.New("observation time regressed")
		}
	} else if !w.BootStartedAt.After(previous.BootStartedAt) {
		return false, nil
	}
	return true, nil
}

// Ingest accepts only the documented schema for an authenticated tenant and an
// enrolled identity. False with nil error is an ignored duplicate/stale packet;
// its arrival does not refresh the heartbeat clock.
func (b *Backend) Ingest(tenant, name string, attrs map[string]any) (bool, error) {
	switch name {
	case HeartbeatEvent:
		h, err := DecodeHeartbeat(attrs)
		if err != nil {
			return false, err
		}
		return b.heartbeat(tenant, h)
	case FeatureEvent:
		f, err := DecodeFeatureUsage(attrs)
		if err != nil {
			return false, err
		}
		return b.feature(tenant, f)
	default:
		return false, errors.New("unsupported diagnostic event")
	}
}
func (b *Backend) heartbeat(tenant string, h Heartbeat) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, err := b.enrolled(tenant, h.Identity)
	if err != nil {
		return false, err
	}
	var prior *Window
	if e.heartbeat != nil {
		prior = &e.heartbeat.Window
	}
	accept, err := b.admissible(h.Window, prior)
	if !accept || err != nil {
		return accept, err
	}
	if e.heartbeat != nil && h.BootID != e.heartbeat.BootID {
		e.features = map[string]FeatureUsage{}
	}
	e.heartbeat = &h
	e.receivedAt = b.now()
	recordTransition(e, evaluate(e, b.tenants[tenant], b.now(), b.maxClockSkew), b.now())
	return true, nil
}
func (b *Backend) feature(tenant string, f FeatureUsage) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, err := b.enrolled(tenant, f.Identity)
	if err != nil {
		return false, err
	}
	// Features cannot resurrect a retired boot or establish deployment liveness.
	if e.heartbeat == nil || f.BootID != e.heartbeat.BootID || !f.BootStartedAt.Equal(e.heartbeat.BootStartedAt) {
		return false, errors.New("feature window has no matching heartbeat boot")
	}
	var prior *Window
	if old, ok := e.features[f.FeatureID]; ok {
		prior = &old.Window
		if f.WindowEnd.Before(old.WindowEnd) || f.WindowStart.Before(old.WindowStart) || f.WindowStart.Equal(old.WindowStart) && f.Count != nil && old.Count != nil && *f.Count < *old.Count {
			return false, errors.New("feature window/count regressed")
		}
	}
	accept, err := b.admissible(f.Window, prior)
	if !accept || err != nil {
		return accept, err
	}
	e.features[f.FeatureID] = f
	return true, nil
}

func sortedKeys(entries map[Key]*entry) []Key {
	keys := make([]Key, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].DeploymentID != keys[j].DeploymentID {
			return keys[i].DeploymentID < keys[j].DeploymentID
		}
		if keys[i].InstanceID != keys[j].InstanceID {
			return keys[i].InstanceID < keys[j].InstanceID
		}
		return keys[i].GaggleID < keys[j].GaggleID
	})
	return keys
}

func validEnrollment(e Enrollment) error {
	for _, v := range []string{e.Organization, e.Environment, e.DeploymentID, e.InstanceID, e.GaggleID, e.OwnerRef, e.Component} {
		if len(v) > 256 {
			return errors.New("enrollment field too long")
		}
	}
	if e.Pin != "" && (len(e.Pin) > 128 || !semver.IsValid(e.Pin)) {
		return errors.New("invalid version pin")
	}
	if e.MaintenanceStart.IsZero() != e.MaintenanceEnd.IsZero() || !e.MaintenanceEnd.IsZero() && !e.MaintenanceEnd.After(e.MaintenanceStart) {
		return errors.New("invalid maintenance window")
	}
	return nil
}
