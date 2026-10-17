# WIP takeover: missing-verdict shadow telemetry

Status: partial draft, unvalidated, not ready to merge.
Repository: `Agent-Clubhouse/Goobers`.
Branch: `campaign/7462-verdict-shadow`.
Base and last fetched main: `395cf74f45765173ccb2e092aa11f57a1477c019`.
The first checkpoint commit contains the four draft files and this note.
There is no PR for this branch.

## Draft contents

- `internal/prstatus/observation.go`: classifies an already-completed provider
  poll as present, absent, unsupported or unreadable; proposes counting only an
  absent verdict that otherwise reached a landing attempt as a would-block.
- `cmd/goobers/mergeshadow.go`: proposes a best-effort instance annotation,
  using existing journal retention and pod transport.
- `cmd/goobers/mergepr.go`: connects that observation to the locked live poll
  and records it after the existing landing decision.
- `test/cmdgoobersgrowth/justifications/7462-verdict-shadow.md`: draft rationale.

This is intended to be shadow-only. It must not change a merge decision, publish
a status, activate enforcement, or silently treat unsupported ADO reads as an
absent verdict. No rollout or policy change occurred.

## Validation and next steps

No formatting, build, tests, policy gates, independent review or remote CI have
been run on this draft. No test files were written. Source may require changes;
the proposed provider reader-capability inference and telemetry placement still
need review. The ledger branch's passing tests do not validate this branch.

Before proceeding, add tests for present/absent/unsupported/unreadable evidence,
unchanged merge behavior, opt-out/advisory/error cases, telemetry failure, and
the production retention path. Confirm journal defaults and retention wiring.
Then run formatting, focused tests, growth/complexity checks and the normal
validation tier. Do not enable enforcement as part of this checkpoint.

The related ledger work is on `campaign/7464-atomic-ledger` and continues existing
PR https://github.com/Agent-Clubhouse/Goobers/pull/7464; this draft does not depend
on an assumption that it landed. Other merge-safety issues remain separate.

Mason requested that all in-flight work be committed and pushed for takeover,
including unfinished work. This checkpoint records that work honestly; it is
not a merge, release or live-policy authorization. Local context is preserved
outside Git in `campaign/merge-ledger-receipt.md` in the originating task
workspace, which is not reproducible from this repository.
