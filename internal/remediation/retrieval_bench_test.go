package remediation

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/api/integrity"
)

// BenchmarkRetrieve measures retrieval only: index construction and scrubbing
// of the corpus are outside the timed loop. Every record is a candidate, so
// comparing K=3 with K=all exposes the cost of formatting discarded examples.
func BenchmarkRetrieve(b *testing.B) {
	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	query := Query{Stage: "review", ErrorClass: "timeout", FailureExcerpt: "review timeout", ConfigDigest: "current"}
	for _, size := range []int{10, 100, 1000} {
		for _, longExcerpts := range []bool{false, true} {
			excerptSize := "short"
			failure, fix := "review timeout", "retry the bounded poll"
			if longExcerpts {
				excerptSize = "bounded"
				failure = strings.Repeat("review timeout ", excerptLimit/15+1)
				fix = strings.Repeat("retry the bounded poll ", excerptLimit/23+1)
			}
			records := make([]Record, size)
			for n := range records {
				records[n] = Record{
					ID: fmt.Sprintf("record-%04d", size-n), Stage: query.Stage, ErrorClass: query.ErrorClass,
					FailureExcerpt: failure, FixExcerpt: fix, DidItHelp: n%4 != 0, OutcomeKnown: true,
					ObservedAt:   now.Add(-time.Duration(n%180) * 24 * time.Hour),
					ConfigDigest: query.ConfigDigest, Integrity: integrity.Trusted,
				}
			}
			index := NewIndex(records, nil)
			for _, k := range []int{DefaultK, size} {
				b.Run(fmt.Sprintf("N=%d/%s/K=%d", size, excerptSize, k), func(b *testing.B) {
					options := Options{K: k, Now: now}
					b.ReportAllocs()
					for b.Loop() {
						if got := index.Retrieve(query, options); len(got) != k {
							b.Fatalf("got %d results, want %d", len(got), k)
						}
					}
				})
			}
		}
	}
}
