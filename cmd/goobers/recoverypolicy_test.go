package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

// writeRecoveryPolicyInstance writes a minimal instance.yaml, optionally
// declaring retention.recovery.maxSnapshots, and returns its layout.
func writeRecoveryPolicyInstance(t *testing.T, maxSnapshots int) instance.Layout {
	t.Helper()
	root := t.TempDir()
	body := "apiVersion: goobers.dev/v1alpha1\nkind: Instance\n"
	if maxSnapshots > 0 {
		body += "retention:\n  recovery:\n    maxSnapshots: " + strconv.Itoa(maxSnapshots) + "\n"
	}
	if err := os.WriteFile(filepath.Join(root, instance.ConfigFileName), []byte(body), 0o600); err != nil {
		t.Fatalf("write instance.yaml: %v", err)
	}
	layout := instance.NewLayout(root)
	if _, err := instance.LoadConfig(layout.ConfigFile()); err != nil {
		t.Fatalf("fixture instance.yaml does not load: %v", err)
	}
	return layout
}

// TestResolveRecoveryPolicyPrefersInstanceConfig is #5092's root cause stated
// as a test: a caller holding a config with no retention section must still
// enforce the operator's configured cap against the instance-wide inventory,
// not DefaultRecoverySnapshotMaxCount. On the live instance those two numbers
// were 3072 and 128 against the same directory, and cleanup was refused as
// "130 of 128 slots used" while `goobers status` reported 2306/3072.
func TestResolveRecoveryPolicyPrefersInstanceConfig(t *testing.T) {
	layout := writeRecoveryPolicyInstance(t, 3072)

	for name, carried := range map[string]*instance.Config{
		"no carried config":              nil,
		"carried config without section": {},
		"carried config with stale section": {
			Retention: instance.RetentionConfig{Recovery: &instance.RecoverySnapshotConfig{MaxSnapshots: 128}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			policy, origin := resolveRecoveryPolicy(layout, carried)
			if !origin.Configured() {
				t.Fatalf("origin = %+v; want source %q", origin, recoveryPolicyFromInstanceConfig)
			}
			if got := policy.MaxSnapshotsEffective(); got != 3072 {
				t.Fatalf("MaxSnapshotsEffective() = %d; want the configured 3072", got)
			}
		})
	}
}

// An instance.yaml that declares no recovery section IS a configured
// resolution: every path reads the same defaults from the same file, so there
// is nothing to report.
func TestResolveRecoveryPolicyUndeclaredSectionIsConfigured(t *testing.T) {
	layout := writeRecoveryPolicyInstance(t, 0)
	policy, origin := resolveRecoveryPolicy(layout, nil)
	if !origin.Configured() || origin.LoadErr != nil {
		t.Fatalf("origin = %+v; want a configured resolution", origin)
	}
	if got := policy.MaxSnapshotsEffective(); got != instance.DefaultRecoverySnapshotMaxCount {
		t.Fatalf("MaxSnapshotsEffective() = %d; want %d", got, instance.DefaultRecoverySnapshotMaxCount)
	}
}

// With instance.yaml unreadable the carried config is used rather than the
// built-in default — it is still an operator value — and the fallback is
// reported with the reason the file could not be read.
func TestResolveRecoveryPolicyFallsBackToCarriedConfig(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	carried := &instance.Config{Retention: instance.RetentionConfig{Recovery: &instance.RecoverySnapshotConfig{MaxSnapshots: 2048}}}

	policy, origin := resolveRecoveryPolicy(layout, carried)
	if origin.Source != recoveryPolicyFromCarriedConfig || origin.Configured() {
		t.Fatalf("origin = %+v; want source %q", origin, recoveryPolicyFromCarriedConfig)
	}
	if origin.LoadErr == nil {
		t.Fatal("fallback did not report why instance.yaml was not the source")
	}
	if got := policy.MaxSnapshotsEffective(); got != 2048 {
		t.Fatalf("MaxSnapshotsEffective() = %d; want the carried 2048", got)
	}
}

// Reaching the built-in defaults is reportable. Before #5092 it was
// indistinguishable from an operator who configured 128, which is why five
// cap increases on the live instance changed nothing.
func TestResolveRecoveryPolicyReportsBuiltInDefault(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	policy, origin := resolveRecoveryPolicy(layout, &instance.Config{})
	if origin.Source != recoveryPolicyFromDefaults || origin.Configured() {
		t.Fatalf("origin = %+v; want source %q", origin, recoveryPolicyFromDefaults)
	}
	if got := policy.MaxSnapshotsEffective(); got != instance.DefaultRecoverySnapshotMaxCount {
		t.Fatalf("MaxSnapshotsEffective() = %d; want %d", got, instance.DefaultRecoverySnapshotMaxCount)
	}
}

type recordingPublicationJournal struct {
	events []journal.Event
	err    error
}

func (r *recordingPublicationJournal) Append(event journal.Event) error {
	r.events = append(r.events, event)
	return r.err
}

func TestJournalRecoveryPolicyFallbackReportsLimitAndRoot(t *testing.T) {
	log := &recordingPublicationJournal{err: errors.New("journal unavailable")}
	origin := recoveryPolicyOrigin{Source: recoveryPolicyFromDefaults, LoadErr: errors.New("read instance.yaml: no such file")}

	// A journal failure must not propagate: the resolution governs a cleanup
	// that is about to run either way.
	journalRecoveryPolicyFallback(log, origin, instance.RecoverySnapshotConfig{}, "/var/lib/goobers/recovery")

	if len(log.events) != 1 {
		t.Fatalf("appended %d events; want exactly 1", len(log.events))
	}
	event := log.events[0]
	if event.Error == nil || event.Error.Code != "recovery_policy_fallback" {
		t.Fatalf("event = %+v; want a recovery_policy_fallback error", event)
	}
	for _, want := range []string{recoveryPolicyFromDefaults, "128", "/var/lib/goobers/recovery", "no such file"} {
		if !strings.Contains(event.Error.Message, want) {
			t.Fatalf("message %q does not name %q", event.Error.Message, want)
		}
	}
}

func TestJournalRecoveryPolicyFallbackSilentWhenConfigured(t *testing.T) {
	log := &recordingPublicationJournal{}
	journalRecoveryPolicyFallback(log, recoveryPolicyOrigin{Source: recoveryPolicyFromInstanceConfig}, instance.RecoverySnapshotConfig{}, "/root/recovery")
	if len(log.events) != 0 {
		t.Fatalf("configured resolution journaled %d events; want none", len(log.events))
	}
}

// TestRecoveryCleanupRequestUsesConfiguredInventoryCap is the assertion the
// live investigation asked for by name: the cleanup guard's effective
// MaxSnapshots must equal the loaded instance configuration, including when
// the config the guard was wired with carries no retention section at all.
func TestRecoveryCleanupRequestUsesConfiguredInventoryCap(t *testing.T) {
	layout := writeRecoveryPolicyInstance(t, 3072)
	log := &recordingPublicationJournal{}

	request, err := recoveryCleanupRequest(
		layout,
		&instance.Config{}, // the retention-less config #5092 traced the 128 to
		filepath.Join(layout.Root, "workcopies"),
		nil,
		"github.com/goobers/goobers",
		worktree.CleanupTarget{OwnerRunID: "0123456789abcdef0123456789abcdef", BaseRef: "refs/heads/main"},
		time.Now().UTC(),
		log,
	)
	if err != nil {
		t.Fatalf("recoveryCleanupRequest: %v", err)
	}
	if request.MaxSnapshots != 3072 {
		t.Fatalf("request.MaxSnapshots = %d; want the configured 3072", request.MaxSnapshots)
	}
	if request.InventoryRoot != filepath.Join(layout.Root, "recovery") {
		t.Fatalf("request.InventoryRoot = %q; want the instance-wide inventory", request.InventoryRoot)
	}
	if len(log.events) != 0 {
		t.Fatalf("configured resolution journaled %d events; want none", len(log.events))
	}
}
