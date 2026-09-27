package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
)

// readConfiguredRecoveryInventory is the single strict reader for the
// instance-wide inventory. It resolves the operator's policy at the point of
// use so raising maxSnapshots unblocks a live daemon without a restart.
func readConfiguredRecoveryInventory(ctx context.Context, layout instance.Layout) ([]recovery.InventoryEntry, int, error) {
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		return nil, 0, fmt.Errorf("load recovery inventory configuration: %w", err)
	}
	limit := cfg.Retention.RecoveryEffective().MaxSnapshotsEffective()
	entries, err := recovery.ReadInventory(ctx, filepath.Join(layout.Root, "recovery"), limit)
	if err != nil {
		err = recoveryInventoryReadError(err, limit)
		return nil, limit, err
	}
	return entries, limit, nil
}

// observeRecoveryInventory reads the whole inventory directory for a caller
// that only OBSERVES it, and returns the operator cap alongside the reading
// rather than enforcing it.
//
// readConfiguredRecoveryInventory stays strict and cap-bounded for the callers
// that decide whether work is safe to discard because recovery state appears
// absent. An observer needs the opposite: bounding the read by the cap makes
// capacity reporting go blind at exactly the occupancy it exists to report,
// because the read itself refuses with "75 of 8 slots used" (#5354). Tolerant
// for the same reason the health sampler is: one crashed publish leaving a
// lock-only directory must not take the reading down with it. Unreadable
// entries are returned, never dropped, because they hold slots.
func observeRecoveryInventory(ctx context.Context, layout instance.Layout) ([]recovery.InventoryEntry, []recovery.UnreadableEntry, int, error) {
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		return nil, nil, 0, fmt.Errorf("load recovery inventory configuration: %w", err)
	}
	limit := cfg.Retention.RecoveryEffective().MaxSnapshotsEffective()
	root := filepath.Join(layout.Root, "recovery")
	entries, unreadable, readErr := recovery.ReadInventoryTolerant(ctx, root, recovery.MaxInventoryEntries)
	if readErr != nil {
		return nil, nil, limit, recoveryInventoryReadError(readErr, limit)
	}
	return entries, unreadable, limit, nil
}

// recoveryInventoryReadError keeps an oversized inventory fail-closed while
// giving an operator a path that does not delete or bypass retained evidence.
func recoveryInventoryReadError(err error, limit int) error {
	if !errors.Is(err, recovery.ErrInventoryFull) {
		return err
	}
	return fmt.Errorf(
		"%w; retained evidence was left untouched: temporarily raise retention.recovery.maxSnapshots above the current inventory size, retry the operation, then use recovery-abandon or configured retention instead of deleting recovery files manually (configured maxSnapshots=%d)",
		err, limit,
	)
}

// Every recovery-snapshot policy resolution names where it came from, so a
// path that governs the instance-wide inventory can never disagree with the
// operator's configuration without saying so (#5092).
const (
	// recoveryPolicyFromInstanceConfig is the only correct source: the policy
	// the operator declared in this instance root's instance.yaml.
	recoveryPolicyFromInstanceConfig = "instance-config"
	// recoveryPolicyFromCarriedConfig is a config value the caller was
	// holding when instance.yaml could not be read. It may be stale, but it
	// is still an operator value rather than a built-in constant.
	recoveryPolicyFromCarriedConfig = "carried-config"
	// recoveryPolicyFromDefaults is the built-in fallback. It is the value
	// #5092 was wedged on for days: cleanup refused at "130 of 128 slots" on
	// an instance configured for 3072, because the failing path resolved
	// DefaultRecoverySnapshotMaxCount while `goobers status` — which reads
	// instance.yaml directly — reported the configured cap against the SAME
	// inventory root.
	recoveryPolicyFromDefaults = "built-in-default"
)

// recoveryPolicyOrigin records how an effective recovery-snapshot policy was
// resolved, so a fallback is reportable rather than silent.
type recoveryPolicyOrigin struct {
	Source string
	// LoadErr is why instance.yaml was not the source, when it was not.
	LoadErr error
}

// Configured reports whether the operator's declared policy was used.
func (o recoveryPolicyOrigin) Configured() bool { return o.Source == recoveryPolicyFromInstanceConfig }

// resolveRecoveryPolicy resolves the policy governing the instance-wide
// recovery inventory at <instance root>/recovery.
//
// It reads instance.yaml at the point of use rather than trusting whatever
// *instance.Config the caller happens to be carrying. The inventory is ONE
// directory shared by every gaggle, every runner, the startup crash-orphan
// reap, the retention sweep and `goobers status`; if those resolve different
// caps against it, cleanup is refused as "full" at a count the operator's own
// tooling reports as far below the limit — which is exactly what #5092 was,
// and why raising maxSnapshots five times (128 -> 256 -> 1024 -> 2048 -> 3072)
// never had any effect on the failing path.
//
// Reading at the point of use also means an operator raising the cap applies
// to live cleanups without a daemon restart: instance.yaml is loaded once at
// boot and is NOT re-read by the config reloader, which re-reads only the
// config directory.
//
// cfg is the fallback, not the source: it is used only when instance.yaml
// cannot be read, and only when it actually declares a recovery section. A
// resolution that reaches the built-in defaults is reported as such rather
// than being indistinguishable from an operator who configured 128
// (instance.RetentionConfig.RecoveryConfigured is what makes that
// distinction possible).
func resolveRecoveryPolicy(layout instance.Layout, cfg *instance.Config) (instance.RecoverySnapshotConfig, recoveryPolicyOrigin) {
	loaded, err := instance.LoadConfig(layout.ConfigFile())
	if err == nil {
		// A clean read is authoritative even when it declares no recovery
		// section: the built-in defaults ARE the operator's configuration
		// then, and every other path resolves the same thing from the same
		// file.
		return loaded.Retention.RecoveryEffective(), recoveryPolicyOrigin{Source: recoveryPolicyFromInstanceConfig}
	}
	if cfg != nil && cfg.Retention.RecoveryConfigured() {
		return cfg.Retention.RecoveryEffective(), recoveryPolicyOrigin{Source: recoveryPolicyFromCarriedConfig, LoadErr: err}
	}
	return instance.RecoverySnapshotConfig{}, recoveryPolicyOrigin{Source: recoveryPolicyFromDefaults, LoadErr: err}
}

// journalRecoveryPolicyFallback makes a non-configured resolution visible to
// an operator at the moment it is used, naming the limit and the inventory
// root it will be enforced against (#5092's acceptance criterion that a
// refusal must be explicable without reconstructing state after the fact).
// Configured resolutions are silent: they are the ordinary case.
//
// Best-effort by construction — a policy resolution must not fail the cleanup
// it is about to govern just because the instance journal is unavailable.
func journalRecoveryPolicyFallback(log recovery.PublicationJournal, origin recoveryPolicyOrigin, policy instance.RecoverySnapshotConfig, root string) {
	if log == nil || origin.Configured() {
		return
	}
	message := fmt.Sprintf(
		"recovery policy resolved from %s: maxSnapshots=%d applies to %s",
		origin.Source, policy.MaxSnapshotsEffective(), root,
	)
	if origin.LoadErr != nil {
		message += fmt.Sprintf(" (instance configuration unavailable: %v)", origin.LoadErr)
	}
	_ = log.Append(journal.Event{
		Type: journal.EventError,
		Error: &journal.ErrorDetail{
			Code:    "recovery_policy_fallback",
			Message: message,
		},
	})
}
