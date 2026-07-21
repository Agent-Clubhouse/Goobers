package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/goobers/goobers/internal/worktree"
)

const (
	// EventWorktreeDiskUsage reports one managed worktree's apparent bytes.
	EventWorktreeDiskUsage = "goobers.worktree.disk.usage"
	// EventWorkcopyDiskUsage reports aggregate managed workcopy bytes.
	EventWorkcopyDiskUsage = "goobers.workcopy.disk.usage"
	// EventWorkcopyDiskMeasurementFailed reports an observable measurement gap.
	EventWorkcopyDiskMeasurementFailed = "goobers.workcopy.disk.measurement_failed"
)

// RecordWorkcopyUsage projects a worktree lifecycle measurement onto the
// current task/gate span. Housekeeping and terminal cleanup without a current
// span get a short scheduler span so local journal and rollup export still see
// the measurement.
func (c *Client) RecordWorkcopyUsage(ctx context.Context, measurement worktree.UsageMeasurement) {
	if c == nil {
		return
	}

	current := trace.SpanFromContext(ctx)
	span := Span{span: current, scrubber: c.scrubber}
	standalone := !current.IsRecording()
	if standalone {
		gaggle := measurement.Gaggle
		if gaggle == "" {
			gaggle = "legacy"
		}
		var err error
		_, span, err = c.StartSchedulerSpan(ctx, SchedulerAttributes{
			Gaggle:     gaggle,
			WorkflowID: "instance",
			Action:     "workcopy-" + string(measurement.Operation),
		})
		if err != nil {
			return
		}
	}

	attrs := usageIdentityAttributes(measurement)
	if measurement.WorktreeMeasured {
		span.Event(EventWorktreeDiskUsage, append(attrs,
			attribute.String(emissionKindAttribute, emissionKindMetric),
			attribute.Int64(metricValueAttribute, measurement.WorktreeBytes),
			attribute.String(metricUnitAttribute, "By"),
		)...)
	}
	if measurement.WorkcopyMeasured {
		span.Event(EventWorkcopyDiskUsage, append(attrs,
			attribute.String(emissionKindAttribute, emissionKindMetric),
			attribute.Int64(metricValueAttribute, measurement.WorkcopyBytes),
			attribute.String(metricUnitAttribute, "By"),
			attribute.Int(AttrUnmeasuredWorktrees, measurement.UnmeasuredWorktrees),
		)...)
	}
	if measurement.Err != nil {
		span.Event(EventWorkcopyDiskMeasurementFailed, append(attrs,
			attribute.String(AttrErrorType, fmt.Sprintf("%T", measurement.Err)),
			attribute.String(AttrErrorMessage, measurement.Err.Error()),
		)...)
	}

	if standalone {
		if measurement.Err != nil {
			span.Fail(measurement.Err)
		} else {
			span.Succeed("workcopy disk usage measured")
		}
	}
}

func usageIdentityAttributes(measurement worktree.UsageMeasurement) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String(AttrStorageOperation, string(measurement.Operation)),
	}
	if measurement.Gaggle != "" {
		attrs = append(attrs, attribute.String(AttrGaggle, measurement.Gaggle))
	}
	if measurement.OwnerRunID != "" {
		attrs = append(attrs, attribute.String(AttrRunID, measurement.OwnerRunID))
	}
	if measurement.WorktreeID != "" {
		attrs = append(attrs, attribute.String(AttrWorktreeID, measurement.WorktreeID))
	}
	return attrs
}
