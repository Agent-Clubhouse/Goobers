package telemetry

import (
	"errors"
	"math"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/onecollector"
)

func collectorPointFacts(base onecollector.Facts, attrs attribute.Set, start, end time.Time, kind string, temporality metricdata.Temporality, scrubber journal.Scrubber) onecollector.Facts {
	facts := make(onecollector.Facts, len(base)+12)
	for key, value := range base {
		facts[key] = value
	}
	addCollectorAttrs(facts, "attributes", attrs.ToSlice(), scrubber)
	startNanos := "0"
	if !start.IsZero() {
		startNanos = strconv.FormatInt(start.UnixNano(), 10)
	}
	facts["metric.start_time_unix_nano"] = onecollector.JSON(startNanos)
	facts["metric.time_unix_nano"] = onecollector.JSON(strconv.FormatInt(end.UnixNano(), 10))
	facts["metric.kind"] = onecollector.JSON(kind)
	switch temporality {
	case metricdata.DeltaTemporality:
		facts["metric.temporality"] = onecollector.JSON("delta")
	case metricdata.CumulativeTemporality:
		facts["metric.temporality"] = onecollector.JSON("cumulative")
	}
	facts["timestamp"] = onecollector.JSON(end.UTC().Format(time.RFC3339Nano))
	return facts
}

func offerCollectorPoints[N int64 | float64](pipeline *oneCollectorPipeline, batch *collectorBatch, base onecollector.Facts, points []metricdata.DataPoint[N], kind string, temporality metricdata.Temporality, monotonic *bool) error {
	var errs []error
	for _, point := range points {
		if value := float64(point.Value); math.IsNaN(value) || math.IsInf(value, 0) || monotonic != nil && *monotonic && value < 0 {
			errs = append(errs, errors.New("onecollector: invalid metric value"))
			continue
		}
		facts := collectorPointFacts(base, point.Attributes, point.StartTime, point.Time, kind, temporality, pipeline.scrubber)
		facts["metric.value"] = onecollector.JSON(point.Value)
		switch value := any(point.Value).(type) {
		case int64:
			facts["metric.integer_value"] = onecollector.JSON(value)
		case float64:
			if integer, ok := collectorInteger(value); ok {
				facts["metric.integer_value"] = onecollector.JSON(integer)
			}
		}
		facts["metric.synthetic"] = onecollector.JSON(false)
		facts["metric.sample_index"] = onecollector.JSON("0")
		if monotonic != nil {
			facts["metric.monotonic"] = onecollector.JSON(*monotonic)
		}
		facts["metric.point_id"] = onecollector.JSON(metricPointDigest(facts))
		pipeline.offer(collectorItem{signal: "metric", facts: facts, batch: batch})
	}
	return errors.Join(errs...)
}

func offerCollectorHistograms(pipeline *oneCollectorPipeline, batch *collectorBatch, base onecollector.Facts, histogram metricdata.Histogram[float64]) error {
	var errs []error
	for _, point := range histogram.DataPoints {
		valid := !math.IsNaN(point.Sum) && !math.IsInf(point.Sum, 0)
		for _, bound := range point.Bounds {
			valid = valid && !math.IsNaN(bound) && !math.IsInf(bound, 0)
		}
		min, hasMin := point.Min.Value()
		max, hasMax := point.Max.Value()
		valid = valid && (!hasMin || !math.IsNaN(min) && !math.IsInf(min, 0)) && (!hasMax || !math.IsNaN(max) && !math.IsInf(max, 0))
		if !valid {
			errs = append(errs, errors.New("onecollector: invalid histogram statistic"))
			continue
		}
		facts := collectorPointFacts(base, point.Attributes, point.StartTime, point.Time, "histogram", histogram.Temporality, pipeline.scrubber)
		facts["metric.synthetic"] = onecollector.JSON(true)
		facts["metric.count"] = onecollector.JSON(point.Count)
		facts["metric.sum"] = onecollector.JSON(point.Sum)
		facts["histogram.counts"] = onecollector.JSON(point.BucketCounts)
		facts["histogram.bounds"] = onecollector.JSON(point.Bounds)
		if hasMin {
			facts["metric.min"] = onecollector.JSON(min)
		}
		if hasMax {
			facts["metric.max"] = onecollector.JSON(max)
		}
		facts["metric.point_id"] = onecollector.JSON(metricPointDigest(facts))
		pipeline.offer(collectorItem{signal: "metric", facts: facts, batch: batch})
	}

	return errors.Join(errs...)
}

func collectorInteger(value float64) (int64, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < -0x1p63 || value >= 0x1p63 {
		return 0, false
	}
	return int64(value), true
}
