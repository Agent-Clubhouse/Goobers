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

// FreezeMapRosterInput binds the map definition to the exact producer attempt
// and source artifact already selected by deterministic workflow history.
type FreezeMapRosterInput struct {
	RunID      string                  `json:"runId"`
	MapName    string                  `json:"mapName"`
	Definition apiv1.Map               `json:"definition"`
	Source     journal.FanOutSourceRef `json:"source"`
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

// FreezeMapRoster reads and verifies the exact source artifact, canonicalizes
// its roster, and stores the normalized roster under its content digest.
func (a *Activities) FreezeMapRoster(ctx context.Context, in FreezeMapRosterInput) (FreezeMapRosterResult, error) {
	if in.RunID == "" || in.MapName == "" || in.Definition.Name != in.MapName {
		return FreezeMapRosterResult{}, invalidMapRosterActivity(
			fmt.Errorf("run ID and map name are required, and definition name must match map name"),
		)
	}
	if a.MapRosters == nil {
		return FreezeMapRosterResult{}, classifySeamError(ErrNotConfigured)
	}
	sourceData, err := a.MapRosters.Get(ctx, in.Source.Digest)
	if err != nil {
		return FreezeMapRosterResult{}, fmt.Errorf("read map %q source %s: %w", in.MapName, in.Source.Digest, err)
	}
	blob, data, digest, err := fanout.FreezeRosterBlob(in.Definition, in.Source, sourceData)
	if err != nil {
		return FreezeMapRosterResult{}, invalidMapRosterActivity(err)
	}
	if _, err := fanout.DecodeRosterBlob(data, digest, in.Definition); err != nil {
		return FreezeMapRosterResult{}, invalidMapRosterActivity(err)
	}
	if err := a.MapRosters.Put(ctx, digest, data); err != nil {
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

func invalidMapRosterActivity(err error) error {
	return temporal.NewNonRetryableApplicationError(err.Error(), "map_roster_invalid", err)
}
