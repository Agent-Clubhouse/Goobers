package main

import (
	"strings"

	"go.opentelemetry.io/otel/attribute"

	"github.com/goobers/goobers/internal/instance"
)

// telemetryInstanceIdentities keeps the historical journal-attribution ID
// visible for migrated roots while root lifecycle and new journals use the
// canonical ID. Observation never adopts or repairs a root.
func telemetryInstanceIdentities(root string) (string, []attribute.KeyValue) {
	if strings.TrimSpace(root) == "" {
		return "", nil
	}
	var journalID string
	var attrs []attribute.KeyValue
	var canonicalID string
	if id, err := instance.NewLayout(root).ReadIdentity(); err == nil {
		canonicalID = id
		journalID = id
	}
	if id, err := instance.ReadLegacyIdentity(root); err == nil {
		if canonicalID == "" {
			journalID = id
		} else if resolved, resolveErr := instance.ResolveRootIdentity(root, id); resolveErr == nil && resolved == canonicalID {
			journalID = id
		}
	}
	if journalID != "" {
		attrs = append(attrs, attribute.String("goobers.instance.id", journalID))
	}
	if canonicalID != "" {
		attrs = append(attrs, attribute.String("goobers.root.id", canonicalID))
	}
	return journalID, attrs
}
