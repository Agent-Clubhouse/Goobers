package main

import (
	"strings"

	"go.opentelemetry.io/otel/attribute"

	"github.com/goobers/goobers/internal/instance"
)

// telemetryInstanceIdentities bridges the existing journal-attribution ID and
// root-lifecycle ID without rotating either. Older service-health instanceId
// fields refer to the latter, whereas run journals use the former. Both signal
// pipelines must publish the same resource keys so a tenant can join them.
// Observation never adopts a legacy root or repairs an unreadable identity.
func telemetryInstanceIdentities(root string) (string, []attribute.KeyValue) {
	if strings.TrimSpace(root) == "" {
		return "", nil
	}
	var journalID string
	var attrs []attribute.KeyValue
	if id, err := instance.NewLayout(root).ReadIdentity(); err == nil {
		journalID = id
		attrs = append(attrs, attribute.String("goobers.instance.id", id))
	}
	if id, err := instance.ReadRootIdentity(root); err == nil {
		attrs = append(attrs, attribute.String("goobers.root.id", id))
	}
	return journalID, attrs
}
