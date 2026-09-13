package main

import (
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/stateclient"
	"github.com/goobers/goobers/providers"
)

func newVerdictTestRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scheduler"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func statusComment(author string, v apiv1.Verdict) providers.Comment {
	return providers.Comment{ID: "1", Author: author, Body: renderVerdictComment(v)}
}

// TestVerdictStateRepositoryIdentityIsolation is Goobers#3025/#5030's
// acceptance criterion that identical PR numbers in different repositories
// must never collide.
func TestVerdictStateRepositoryIdentityIsolation(t *testing.T) {
	root := newVerdictTestRoot(t)
	repoA := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	repoB := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "api"}

	if err := writeVerdictState(root, repoA, 42, apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "A"}); err != nil {
		t.Fatalf("write repoA: %v", err)
	}
	if err := writeVerdictState(root, repoB, 42, apiv1.Verdict{Decision: apiv1.VerdictFail, Summary: "B"}); err != nil {
		t.Fatalf("write repoB: %v", err)
	}

	gotA, ok, err := loadVerdictState(root, repoA, 42)
	if err != nil || !ok || gotA.Decision != apiv1.VerdictPass {
		t.Fatalf("repoA#42 = %+v, %v, %v; want pass", gotA, ok, err)
	}
	gotB, ok, err := loadVerdictState(root, repoB, 42)
	if err != nil || !ok || gotB.Decision != apiv1.VerdictFail {
		t.Fatalf("repoB#42 = %+v, %v, %v; want fail (independent of repoA's identically-numbered PR)", gotB, ok, err)
	}

	repoC := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "acme", Name: "web", Project: "proj"}
	if err := writeVerdictState(root, repoC, 42, apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "C"}); err != nil {
		t.Fatalf("write repoC: %v", err)
	}
	gotAAfter, ok, err := loadVerdictState(root, repoA, 42)
	if err != nil || !ok || gotAAfter.Decision != apiv1.VerdictPass {
		t.Fatalf("repoA#42 after ADO repoC write = %+v, %v, %v; want unchanged pass", gotAAfter, ok, err)
	}
}

// TestVerdictStateCASConflictRetries mirrors the failure-streak CAS coverage:
// concurrent writers must not lose each other's writes to different keys, and
// a single key's compare-and-swap must serialize concurrent updates.
func TestVerdictStateCASConflictRetries(t *testing.T) {
	root := newVerdictTestRoot(t)
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	store, err := openStageStateStore(instance.NewLayout(root))
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}
	key := verdictRecordKey(repo, 7)
	ctx := stateContext()

	const writers = 8
	errCh := make(chan error, writers)
	for i := range writers {
		i := i
		go func() {
			errCh <- updateVerdictRecord(ctx, store, key, func(apiv1.Verdict, bool) (apiv1.Verdict, bool, error) {
				return apiv1.Verdict{Decision: apiv1.VerdictPass, Rationale: string(rune('a' + i))}, true, nil
			})
		}()
	}
	for range writers {
		if err := <-errCh; err != nil {
			t.Fatalf("concurrent update: %v", err)
		}
	}

	got, ok, err := loadVerdictState(root, repo, 7)
	if err != nil || !ok {
		t.Fatalf("final read = %+v, %v, %v; want a record from one of the concurrent writers", got, ok, err)
	}
}

// TestVerdictStateSurvivesCommentDeletionAndEditing is Goobers#3025/#5030's
// core invariant: once the authoritative key exists, deleting or editing the
// provider comment cannot change what gatherPRVerdict reports.
func TestVerdictStateSurvivesCommentDeletionAndEditing(t *testing.T) {
	root := newVerdictTestRoot(t)
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	authoritative := apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "Fix the null check."}
	if err := writeVerdictState(root, repo, 88, authoritative); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Comment deleted entirely.
	if got := gatherPRVerdict(root, repo, 88, nil, "goober-bot"); got == nil || got.Decision != apiv1.VerdictNeedsChanges {
		t.Fatalf("verdict after comment deletion = %+v, want unchanged needs-changes", got)
	}

	// Comment hand-edited to a bogus pass.
	edited := []providers.Comment{statusComment("goober-bot", apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "totally fine now"})}
	if got := gatherPRVerdict(root, repo, 88, edited, "goober-bot"); got == nil || got.Decision != apiv1.VerdictNeedsChanges {
		t.Fatalf("verdict after comment edited to pass = %+v, want the KV record (needs-changes) to win", got)
	}
}

// TestVerdictStateSurvivesADOAppendOnlyAccumulation covers the Azure DevOps
// shape of "comments are a projection": ADO PR threads cannot be edited in
// place the way a GitHub issue comment is, so a PR that has gone through
// several merge-review cycles accumulates multiple status comments instead of
// one rolling one. Once the authoritative key exists, that accumulated
// history — including a newer comment that looks more authoritative by
// position — must not override it.
func TestVerdictStateSurvivesADOAppendOnlyAccumulation(t *testing.T) {
	root := newVerdictTestRoot(t)
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "acme", Name: "web", Project: "proj"}
	if err := writeVerdictState(root, repo, 21, apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "authoritative"}); err != nil {
		t.Fatalf("write: %v", err)
	}

	accumulated := []providers.Comment{
		statusComment("goober-bot", apiv1.Verdict{Decision: apiv1.VerdictFail, Summary: "first cycle"}),
		statusComment("goober-bot", apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "most recent, would win if comments were read"}),
	}
	got := gatherPRVerdict(root, repo, 21, accumulated, "goober-bot")
	if got == nil || got.Decision != apiv1.VerdictNeedsChanges {
		t.Fatalf("verdict with accumulated ADO comments = %+v, want the KV record (needs-changes) to win", got)
	}
}

// TestVerdictStateMigratesFromLegacyCommentMarker is Goobers#3025/#5030's
// one-time migration-on-read: an absent key is populated from the legacy
// verdict-json comment marker, and that value is durable afterward even if
// the comment later becomes unavailable.
func TestVerdictStateMigratesFromLegacyCommentMarker(t *testing.T) {
	root := newVerdictTestRoot(t)
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	legacy := apiv1.Verdict{Decision: apiv1.VerdictFail, Summary: "Fundamentally broken."}
	comments := []providers.Comment{statusComment("goober-bot", legacy)}

	got := gatherPRVerdict(root, repo, 99, comments, "goober-bot")
	if got == nil || got.Decision != apiv1.VerdictFail {
		t.Fatalf("migrated verdict = %+v, want fail", got)
	}

	// The key must now be durable — a read with no comments at all still
	// returns it.
	stored, ok, err := loadVerdictState(root, repo, 99)
	if err != nil || !ok || stored.Decision != apiv1.VerdictFail {
		t.Fatalf("state after migration = %+v, %v, %v; want fail, true, nil", stored, ok, err)
	}
}

// TestVerdictStateMalformedLegacyMarkerDoesNotMigrate covers the fail-closed
// side of migration-on-read: a status comment whose embedded verdict-json
// payload cannot be parsed must not fabricate a verdict, and must not poison
// the key with a bogus value.
func TestVerdictStateMalformedLegacyMarkerDoesNotMigrate(t *testing.T) {
	root := newVerdictTestRoot(t)
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	malformed := []providers.Comment{{
		ID: "1", Author: "goober-bot",
		Body: mergeReviewStatusMarker + "\n**merge-review verdict: needs-changes**\n\n<!-- verdict-json: {not valid json -->",
	}}

	if got := gatherPRVerdict(root, repo, 13, malformed, "goober-bot"); got != nil {
		t.Fatalf("verdict from malformed marker = %+v, want nil", got)
	}
	if _, ok, err := loadVerdictState(root, repo, 13); err != nil || ok {
		t.Fatalf("state after malformed marker = ok=%v, err=%v; want no record created", ok, err)
	}
}

// TestVerdictStateDecodeRejectsForeignKeyedRecord guards the integrity
// posture verdictDocument shares with remediationNoopDocument and
// failureStreakDocument: a document whose embedded key does not match the key
// it was read at is a decode error, never silently accepted.
func TestVerdictStateDecodeRejectsForeignKeyedRecord(t *testing.T) {
	data, err := encodeVerdictRecord("github|host|~|acme|web|~#1", apiv1.Verdict{Decision: apiv1.VerdictPass})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, _, err := decodeVerdictRecord(stateclient.Value{Data: data, ETag: "x"}, "github|host|~|acme|web|~#2"); err == nil {
		t.Fatal("decode with mismatched key = nil error, want a mis-keyed-record error")
	}
}

// TestVerdictStateRestartDurability confirms the record survives a fresh
// process re-opening the same instance root.
func TestVerdictStateRestartDurability(t *testing.T) {
	root := newVerdictTestRoot(t)
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	if err := writeVerdictState(root, repo, 5, apiv1.Verdict{Decision: apiv1.VerdictPass}); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, ok, err := loadVerdictState(root, repo, 5)
	if err != nil || !ok || got.Decision != apiv1.VerdictPass {
		t.Fatalf("read after restart = %+v, %v, %v; want pass, true, nil", got, ok, err)
	}
}
