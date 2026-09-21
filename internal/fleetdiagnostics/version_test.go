package fleetdiagnostics

import (
	"testing"
	"time"
)

func TestApprovedVersionFreshness(t *testing.T) {
	tests := []struct {
		name, version, channel, pin, want string
		unavailable                       bool
	}{
		{"current", "v0.5.0", "stable", "", "current", false},
		{"outdated", "v0.4.1", "stable", "", "outdated", false},
		{"pinned", "v0.4.1", "stable", "v0.4.1", "pinned", false},
		{"pin mismatch", "v0.4.0", "stable", "v0.4.1", "pin-mismatch", false},
		{"preview approved", "v0.5.0-rc.1", "preview", "", "current", false},
		{"preview behind stable", "v0.5.0-rc.1", "stable", "", "outdated", false},
		{"unknown build", "dev", "stable", "", "unknown", false},
		{"unknown channel", "v0.5.0", "experimental", "", "unknown", false},
		{"catalogue unavailable", "v0.4.1", "stable", "", "unknown", true},
		{"ahead", "v0.6.0", "stable", "", "ahead", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			now := testTime
			b := backendFixture(t, &now)
			e := b.entries["tenant-a"][Key{"deployment", "instance", "gaggle"}].enrollment
			e.Pin = tc.pin
			if err := b.Enroll("tenant-a", e); err != nil {
				t.Fatal(err)
			}
			p := b.tenants["tenant-a"]
			p.Catalogue.Unavailable = tc.unavailable
			if err := b.SetTenant("tenant-a", p); err != nil {
				t.Fatal(err)
			}
			a := heartbeatFields(now)
			a["version"] = tc.version
			a["channel"] = tc.channel
			ingest(t, b, HeartbeatEvent, a)
			got := oneReport(t, b).Freshness
			if got.Status != tc.want || got.Source == "" || !got.ComparedAt.Equal(now) {
				t.Fatal(got)
			}
		})
	}
}
func TestMaintenanceAndCatalogueAge(t *testing.T) {
	now := testTime
	b := backendFixture(t, &now)
	e := b.entries["tenant-a"][Key{"deployment", "instance", "gaggle"}].enrollment
	e.MaintenanceStart = now.Add(time.Hour)
	e.MaintenanceEnd = now.Add(2 * time.Hour)
	if err := b.Enroll("tenant-a", e); err != nil {
		t.Fatal(err)
	}
	a := heartbeatFields(now)
	a["version"] = "v0.4.1"
	ingest(t, b, HeartbeatEvent, a)
	for _, tc := range []struct {
		after time.Duration
		want  string
	}{{0, "scheduled"}, {time.Hour, "maintenance"}, {2 * time.Hour, "outdated"}, {25 * time.Hour, "unknown"}} {
		now = testTime.Add(tc.after)
		if got := oneReport(t, b).Freshness; got.Status != tc.want {
			t.Fatalf("after %s: %+v", tc.after, got)
		}
	}
}
