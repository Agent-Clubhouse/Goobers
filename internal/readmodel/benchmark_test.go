package readmodel

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

var benchmarkProjection Projection

func BenchmarkProjectRun(b *testing.B) {
	identity := testIdentity()
	events := completedRunEvents()

	b.ReportAllocs()
	for b.Loop() {
		benchmarkProjection = ProjectRun(identity, Projection{}, events)
	}
}

// BenchmarkRebuildValidation records that validation stays at three queries as
// the number of live runs grows, rather than adding queries per run.
func BenchmarkRebuildValidation(b *testing.B) {
	for _, runCount := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("runs=%d", runCount), func(b *testing.B) {
			ctx := context.Background()
			store, err := Open(filepath.Join(b.TempDir(), "read.db"))
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = store.Close() })

			rebuild, err := store.BeginRebuild(ctx)
			if err != nil {
				b.Fatal(err)
			}
			for i := 0; i < runCount; i++ {
				runID := fmt.Sprintf("%032x", i)
				run := completed(runID, uint64(i+1))
				if err := store.UpsertRun(ctx, run); err != nil {
					b.Fatal(err)
				}
				if err := rebuild.Target().UpsertRun(ctx, run); err != nil {
					b.Fatal(err)
				}
			}
			b.Cleanup(func() { _ = rebuild.Abort() })

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if err := rebuild.assertNoRegression(ctx); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(3, "queries/validation")
			b.ReportMetric(float64(runCount), "runs")
		})
	}
}
