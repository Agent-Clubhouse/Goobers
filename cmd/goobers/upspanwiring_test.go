package main

// upspanwiring_test.go pins the DAEMON STARTUP half of #3805: `goobers up`
// must hand the SAME blob store to both span consumers.
//
// The two constructors below have their own tests — a writer given a store
// adopts spans, a reconciler given one verifies them without divergence — and
// both of those pass with a daemon that hands neither of them anything. The
// argument at the call site is the whole defect: #3805 is titled
// "WithSpanSource has only test-only callers", and a fix whose production
// call sites are themselves uncalled by any test is the same bug one level up.

import (
	"bytes"
	"context"
	"testing"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/readmodel/intake"
	"github.com/goobers/goobers/internal/telemetry"
)

// TestUpHandsTheDaemonBlobStoreToBothSpanConsumers runs the real startup path
// (a pre-cancelled context, the same shape TestUpDisableReadModelReadsFlag
// StartsCleanly uses) with both constructors wrapped, and asserts what up.go
// passed them.
//
// ONE store, not two equal ones: DS5 verifies a live-authored journal against
// a re-projection, so a source given to the writer and not to the reconciler
// turns every adopted span into a false live_journal_divergence — the
// half-wired daemon is worse than the unwired one.
func TestUpHandsTheDaemonBlobStoreToBothSpanConsumers(t *testing.T) {
	root := initDeterministicDemo(t)

	var (
		writerBlobs     blobstore.Store
		projectionBlobs blobstore.Store
		builtWriter     bool
		startedLoop     bool
	)

	originalWriter := newLiveJournalWriter
	newLiveJournalWriter = func(l instance.Layout, cfg *instance.Config, set *instance.ConfigSet,
		watermarks *intake.Store, instanceLog *journal.InstanceLog, blobs blobstore.Store) (*livejournal.Writer, error) {
		builtWriter, writerBlobs = true, blobs
		return originalWriter(l, cfg, set, watermarks, instanceLog, blobs)
	}
	t.Cleanup(func() { newLiveJournalWriter = originalWriter })

	originalProjection := startEngineProjection
	startEngineProjection = func(ctx context.Context, l instance.Layout, cfg *instance.Config, set *instance.ConfigSet,
		watermarks *intake.Store, instanceLog *journal.InstanceLog, tel *telemetry.Client,
		liveJournals *livejournal.Writer, blobs blobstore.Store) (func(), error) {
		startedLoop, projectionBlobs = true, blobs
		return originalProjection(ctx, l, cfg, set, watermarks, instanceLog, tel, liveJournals, blobs)
	}
	t.Cleanup(func() { startEngineProjection = originalProjection })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	if code := runUpContext(ctx, []string{"--quiet", root}, &stdout, &stderr); code != 0 {
		t.Fatalf("runUpContext code = %d, stderr = %q", code, stderr.String())
	}

	if !builtWriter || !startedLoop {
		t.Fatalf("daemon startup did not reach both span consumers: writer = %t, projection = %t",
			builtWriter, startedLoop)
	}
	if writerBlobs == nil {
		t.Fatal("the daemon built its live journal writer with NO span source: every pod-executed " +
			"agentic stage records error.code=span_unavailable in place of its transcript")
	}
	if projectionBlobs == nil {
		t.Fatal("the daemon started the DS5 reconciler with NO span source: every span the live " +
			"writer adopts re-projects as a normative span_unavailable and is filed as a divergence")
	}
	if writerBlobs != projectionBlobs {
		t.Fatalf("the two sides got different stores (%v vs %v); DS5 compares their output event for event",
			writerBlobs.Describe(), projectionBlobs.Describe())
	}

	// And it is the daemon's own instance-local store — the directory the
	// blob plane serves, which is where a stage pod's PUT lands.
	dir, ok := writerBlobs.(*blobstore.Dir)
	if !ok {
		t.Fatalf("span source is %T, want the daemon's directory-backed store", writerBlobs)
	}
	if want := instance.NewLayout(root).BlobStoreDir(); dir.Root != want {
		t.Fatalf("span source root = %q, want the daemon's own blob store %q", dir.Root, want)
	}
}
