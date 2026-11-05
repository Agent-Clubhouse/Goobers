package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"go.temporal.io/sdk/temporal"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/fanout"
	"github.com/goobers/goobers/internal/journal"
)

// ActFreezeMapRoster is the activity that freezes one journal-bound source
// into the normalized content-addressed roster consumed by the map controller.
const ActFreezeMapRoster = "FreezeMapRoster"

// ActLoadFrozenMapItem loads one canonical item from the frozen roster at
// branch dispatch. The whole roster never enters workflow history.
const ActLoadFrozenMapItem = "LoadFrozenMapItem"

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

// LoadFrozenMapItemInput binds an item load to the compact freeze result
// already recorded in history.
type LoadFrozenMapItemInput struct {
	RunID        string        `json:"runId"`
	MapName      string        `json:"mapName"`
	Definition   apiv1.Map     `json:"definition"`
	RosterDigest string        `json:"rosterDigest"`
	RosterSize   int64         `json:"rosterSize"`
	Index        int           `json:"index"`
	Item         MapRosterItem `json:"item"`
}

// LoadFrozenMapItemResult carries one verified canonical item.
type LoadFrozenMapItemResult struct {
	Item json.RawMessage `json:"item"`
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
		Source: in.Source, Items: items,
	}, nil
}

// LoadFrozenMapItem reads only the frozen content-addressed blob and checks
// the selected item against its recorded descriptor.
func (a *Activities) LoadFrozenMapItem(ctx context.Context, in LoadFrozenMapItemInput) (LoadFrozenMapItemResult, error) {
	if in.RunID == "" || in.MapName == "" || in.Definition.Name != in.MapName ||
		!fanout.ValidDigest(in.RosterDigest) || in.RosterSize < 1 || in.Index < 0 {
		return LoadFrozenMapItemResult{}, invalidFanOutCallerActivity(
			fmt.Errorf("run ID, map identity, frozen roster reference and item index are required"),
		)
	}
	if in.RosterSize > fanout.MaxFrozenRosterBytes {
		return LoadFrozenMapItemResult{}, invalidFanOutSourceActivity(&fanout.SourceError{
			Reason: fanout.ReasonTooLarge,
			Detail: fmt.Sprintf("normalized roster is %d bytes, bound is %d", in.RosterSize, fanout.MaxFrozenRosterBytes),
		})
	}
	if a.FanOutBlobs == nil {
		return LoadFrozenMapItemResult{}, classifySeamError(ErrNotConfigured)
	}
	data, err := a.FanOutBlobs.Get(ctx, in.RosterDigest)
	if err != nil {
		return LoadFrozenMapItemResult{}, fmt.Errorf(
			"read map %q frozen roster %s: %w", in.MapName, in.RosterDigest, err,
		)
	}
	if int64(len(data)) != in.RosterSize {
		return LoadFrozenMapItemResult{}, invalidFanOutSourceActivity(
			fmt.Errorf("frozen map roster size %d differs from recorded %d", len(data), in.RosterSize),
		)
	}
	blob, err := fanout.DecodeRosterBlob(data, in.RosterDigest, in.Definition)
	if err != nil {
		return LoadFrozenMapItemResult{}, invalidFanOutSourceActivity(err)
	}
	if in.Index >= len(blob.Roster.Items) {
		return LoadFrozenMapItemResult{}, invalidFanOutSourceActivity(
			fmt.Errorf("frozen map roster item index %d is outside %d items", in.Index, len(blob.Roster.Items)),
		)
	}
	item := blob.Roster.Items[in.Index]
	if in.Item.Identity != item.Identity || in.Item.Digest != item.Digest {
		return LoadFrozenMapItemResult{}, invalidFanOutSourceActivity(
			fmt.Errorf("frozen map roster item %d differs from recorded descriptor", in.Index),
		)
	}
	return LoadFrozenMapItemResult{Item: item.Item}, nil
}

func invalidFanOutCallerActivity(err error) error {
	return temporal.NewNonRetryableApplicationError(err.Error(), "fanout_caller_invalid", err)
}

func invalidFanOutSourceActivity(err error) error {
	return temporal.NewNonRetryableApplicationError(err.Error(), fanout.CodeSourceInvalid, err)
}
