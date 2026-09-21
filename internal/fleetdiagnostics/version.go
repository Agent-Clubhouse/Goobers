package fleetdiagnostics

import (
	"errors"
	"time"

	"golang.org/x/mod/semver"
)

// Catalogue is supplied by the company, never fetched implicitly upstream.
// Releases maps approved channels to desired SemVer versions.
type Catalogue struct {
	Source      string
	ObservedAt  time.Time
	MaxAge      time.Duration
	Unavailable bool
	Releases    map[string]string
}

func (c Catalogue) validate() error {
	if len(c.Source) > 256 || len(c.Releases) > 32 || c.MaxAge < 0 || c.MaxAge > 30*24*time.Hour {
		return errors.New("invalid release catalogue")
	}
	for channel, version := range c.Releases {
		if channel == "" || len(channel) > 64 || len(version) > 128 || !semver.IsValid(version) {
			return errors.New("invalid catalogue channel/version")
		}
	}
	return nil
}
func (c Catalogue) clone() Catalogue {
	releases := map[string]string{}
	for k, v := range c.Releases {
		releases[k] = v
	}
	c.Releases = releases
	return c
}

// Freshness states the source and time of an explicit comparison. Unknown/dev
// builds and unavailable/stale catalogues never become upgrade alerts.
type Freshness struct {
	Status              string    `json:"status"`
	Observed            string    `json:"observed,omitempty"`
	Desired             string    `json:"desired,omitempty"`
	Source              string    `json:"source,omitempty"`
	CatalogueObservedAt time.Time `json:"catalogueObservedAt"`
	ComparedAt          time.Time `json:"comparedAt"`
	Reason              string    `json:"reason"`
}

func freshness(h *Heartbeat, e Enrollment, c Catalogue, now time.Time) Freshness {
	f := Freshness{Status: "unknown", ComparedAt: now, Source: c.Source, CatalogueObservedAt: c.ObservedAt, Reason: "build_unknown"}
	if h == nil {
		return f
	}
	f.Observed = h.Version
	if !semver.IsValid(h.Version) {
		return f
	}
	if e.Pin != "" {
		f.Source = "operator-pin"
		f.Desired = e.Pin
		f.Reason = "explicit_pin"
		f.Status = "pin-mismatch"
		if h.Version == e.Pin {
			f.Status = "pinned"
		}
		return f
	}
	maxAge := c.MaxAge
	if maxAge == 0 {
		maxAge = 24 * time.Hour
	}
	if c.Unavailable || c.Source == "" || c.ObservedAt.IsZero() || c.ObservedAt.After(now) || now.Sub(c.ObservedAt) > maxAge {
		f.Reason = "catalogue_unavailable"
		return f
	}
	desired, ok := c.Releases[h.Channel]
	if !ok {
		f.Reason = "channel_unknown"
		return f
	}
	f.Desired = desired
	switch semver.Compare(h.Version, desired) {
	case 0:
		f.Status = "current"
		f.Reason = "matches_approved_version"
	case 1:
		f.Status = "ahead"
		f.Reason = "newer_than_approved_version"
	default:
		f.Status = "outdated"
		f.Reason = "older_than_approved_version"
		if now.Before(e.MaintenanceEnd) {
			f.Status = "maintenance"
			f.Reason = "within_maintenance_window"
			if now.Before(e.MaintenanceStart) {
				f.Status = "scheduled"
				f.Reason = "maintenance_window_pending"
			}
		}
	}
	return f
}
