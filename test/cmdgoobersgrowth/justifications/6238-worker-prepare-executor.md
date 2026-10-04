# #6238: shared worker goober invocation prelude

Growth: about +26 non-test lines and no new files in `cmd/goobers`
(`workerwiring.go`).

## Why the growth belongs in the command package

`workerGoober.Invoke` and `workerGoober.Review` previously duplicated the same
prelude: pinned gaggle acquisition, unavailable-harness refusal, executor
resolution, materialization and merge-authority wrapping. This change folds
that prelude into one `prepareExecutor` helper, so the duplicated statements go
away, but the helper adds two things the duplicated code did not have:

- explicit cleanup-ownership transfer (`transferred` plus a deferred release),
  so the pinned gaggle is released exactly once on every setup error or panic
  and handed to the caller only on success; and
- three optional seams on `workerGoober` (`acquire`, `materialize`,
  `mergeAuthority`) that default to the existing `workerSeams` methods and let
  the characterization test drive every setup-error and panic path.

`workerGoober`, `workerSeams` and `gaggleSeams` are the daemon worker's
composition-root wiring and are unexported in `cmd/goobers`; the prelude cannot
move without moving that wiring.

## Could any of it live elsewhere?

No. The helper only re-orders calls on `cmd/goobers`-private types. The net
growth is the cleanup-ownership guard and the test seams, both of which exist
to make the shared prelude provably release-safe.
