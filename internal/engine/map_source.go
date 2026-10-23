package engine

import (
	"context"
	"fmt"

	"go.temporal.io/sdk/temporal"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/fanout"
	"github.com/goobers/goobers/internal/journal"
)

// ActFreezeMapRoster is the activity that freezes one journal-bound source
// into the normalized content-addressed roster consumed by the map controller.
const ActFreezeMapRoster = "FreezeMapRoster"

// ActLoadFrozenMapRoster loads the content-addressed normalized roster after
// its compact descriptor result has been frozen in workflow history.
const ActLoadFrozenMapRoster = "LoadFrozenMapRoster"

const maxFrozenMapRosterBytes = 2 * fanout.MaxSourceBytes

// FreezeMapRosterInput binds the map definition to the exact producer attempt
// and source artifact already selected by deterministic workflow history.
type FreezeMapRosterInput struct {
	RunID          string                  `json:"runId"`
	MapName        string                  `json:"mapName"`
	Definition     apiv1.Map               `json:"definition"`
	Source         journal.FanOutSourceRef `json:"source"`
	MaxSourceBytes int64                   `json:"maxSourceBytes,omitempty"`
}

// MapRosterItem identifies one frozen branch without carrying its item payload
// into Temporal history. The payload remains in the content-addressed roster.
type MapRosterItem struct {
	Identity string `json:"identity"`
	Digest   string `json:"digest"`
}

// FreezeMapRosterResult is the compact, replay-stable activity result. A later
// binding hook resolves RosterDigest and selects the item by descriptor order.
type FreezeMapRosterResult struct {
	MapName      string                  `json:"mapName"`
	RosterDigest string                  `json:"rosterDigest"`
	RosterSize   int64                   `json:"rosterSize"`
	Source       journal.FanOutSourceRef `json:"source"`
	Items        []MapRosterItem         `json:"items"`
}

// LoadFrozenMapRosterInput binds a blob load to the exact compact freeze
// result already recorded in history.
type LoadFrozenMapRosterInput struct {
	RunID        string          `json:"runId"`
	MapName      string          `json:"mapName"`
	Definition   apiv1.Map       `json:"definition"`
	RosterDigest string          `json:"rosterDigest"`
	RosterSize   int64           `json:"rosterSize"`
	Items        []MapRosterItem `json:"items"`
}

// LoadFrozenMapRosterResult carries the verified canonical roster used for
// map.started and fresh per-dispatch binding clones.
type LoadFrozenMapRosterResult struct {
	Roster journal.MapRoster `json:"roster"`
}

// FreezeMapRoster reads and verifies the exact source artifact, canonicalizes
// its roster, and stores the normalized roster under its content digest.
func (a *Activities) FreezeMapRoster(ctx context.Context, in FreezeMapRosterInput) (FreezeMapRosterResult, error) {
	if in.RunID == "" || in.MapName == "" || in.Definition.Name != in.MapName {
		return FreezeMapRosterResult{}, invalidFanOutCallerActivity(
			fmt.Errorf("run ID and map name are required, and definition name must match map name"),
		)
	}
	if a.FanOutBlobs == nil {
		return FreezeMapRosterResult{}, classifySeamError(ErrNotConfigured)
	}
	limit := in.MaxSourceBytes
	if limit <= 0 || limit > fanout.MaxSourceBytes {
		limit = fanout.MaxSourceBytes
	}
	if err := fanout.ValidateSourceBlobRef(in.Source, limit); err != nil {
		return FreezeMapRosterResult{}, invalidFanOutSourceActivity(err)
	}
	sourceData, err := a.FanOutBlobs.Get(ctx, in.Source.Digest)
	if err != nil {
		return FreezeMapRosterResult{}, fmt.Errorf("read map %q source %s: %w", in.MapName, in.Source.Digest, err)
	}
	blob, data, digest, err := fanout.FreezeRosterBlob(in.Definition, in.Source, sourceData)
	if err != nil {
		return FreezeMapRosterResult{}, invalidFanOutSourceActivity(err)
	}
	if _, err := fanout.DecodeRosterBlob(data, digest, in.Definition); err != nil {
		return FreezeMapRosterResult{}, invalidFanOutSourceActivity(err)
	}
	if err := a.FanOutBlobs.Put(ctx, digest, data); err != nil {
		return FreezeMapRosterResult{}, fmt.Errorf("store map %q frozen roster %s: %w", in.MapName, digest, err)
	}
	items := make([]MapRosterItem, len(blob.Roster.Items))
	for i, item := range blob.Roster.Items {
		items[i] = MapRosterItem{Identity: item.Identity, Digest: item.Digest}
	}
	return FreezeMapRosterResult{
		MapName: in.MapName, RosterDigest: digest, RosterSize: int64(len(data)),
		Source: blob.Roster.Source, Items: items,
	}, nil
}

// LoadFrozenMapRoster reads only the frozen content-addressed blob and checks
// it against every descriptor recorded by FreezeMapRoster.
func (a *Activities) LoadFrozenMapRoster(ctx context.Context, in LoadFrozenMapRosterInput) (LoadFrozenMapRosterResult, error) {
	if in.RunID == "" || in.MapName == "" || in.Definition.Name != in.MapName ||
		!fanout.ValidDigest(in.RosterDigest) || in.RosterSize < 1 || in.RosterSize > maxFrozenMapRosterBytes {
		return LoadFrozenMapRosterResult{}, invalidFanOutCallerActivity(
			fmt.Errorf("run ID, map identity and valid bounded roster reference are required"),
		)
	}
	if a.FanOutBlobs == nil {
		return LoadFrozenMapRosterResult{}, classifySeamError(ErrNotConfigured)
	}
	data, err := a.FanOutBlobs.Get(ctx, in.RosterDigest)
	if err != nil {
		return LoadFrozenMapRosterResult{}, fmt.Errorf(
			"read map %q frozen roster %s: %w", in.MapName, in.RosterDigest, err,
		)
	}
	if int64(len(data)) != in.RosterSize {
		return LoadFrozenMapRosterResult{}, invalidFanOutSourceActivity(
			fmt.Errorf("frozen map roster size %d differs from recorded %d", len(data), in.RosterSize),
		)
	}
	blob, err := fanout.DecodeRosterBlob(data, in.RosterDigest, in.Definition)
	if err != nil {
		return LoadFrozenMapRosterResult{}, invalidFanOutSourceActivity(err)
	}
	if len(blob.Roster.Items) != len(in.Items) {
		return LoadFrozenMapRosterResult{}, invalidFanOutSourceActivity(
			fmt.Errorf("frozen map roster item count differs from recorded descriptors"),
		)
	}
	for i, item := range blob.Roster.Items {
		if in.Items[i].Identity != item.Identity || in.Items[i].Digest != item.Digest {
			return LoadFrozenMapRosterResult{}, invalidFanOutSourceActivity(
				fmt.Errorf("frozen map roster item %d differs from recorded descriptor", i),
			)
		}
	}
	return LoadFrozenMapRosterResult{Roster: blob.Roster}, nil
}

func invalidFanOutCallerActivity(err error) error {
	return temporal.NewNonRetryableApplicationError(err.Error(), "fanout_caller_invalid", err)
}

func invalidFanOutSourceActivity(err error) error {
	return temporal.NewNonRetryableApplicationError(err.Error(), fanout.CodeSourceInvalid, err)
}
