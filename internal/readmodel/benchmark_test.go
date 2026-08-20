package readmodel

import "testing"

var benchmarkProjection Projection

func BenchmarkProjectRun(b *testing.B) {
	identity := testIdentity()
	events := completedRunEvents()

	b.ReportAllocs()
	for b.Loop() {
		benchmarkProjection = ProjectRun(identity, Projection{}, events)
	}
}
