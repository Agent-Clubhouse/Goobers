-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

-- Claim history the way the file ledger keeps it (localscheduler.ClaimLedger):
-- one record per run and key, written whenever a lease is taken, renewed or
-- verified and completed on release, so a live or expired-and-overwritten
-- lease has an unreleased record.
--
-- This is an expand: control.claim_history keeps its shape and contents, and
-- current binaries keep appending each released incarnation to it exactly as
-- the previous binary does, so a previous binary can still read and release
-- during a rolling upgrade. History reads merge the two, so a release made by
-- the previous binary still completes the run's record. A later contract
-- migration retires control.claim_history.
CREATE TABLE control.claim_run_history (
    instance_id text NOT NULL,
    run_id text NOT NULL,
    gaggle text NOT NULL,
    provider text NOT NULL,
    external_id text NOT NULL,
    workflow text NOT NULL,
    owner_token text NOT NULL,
    owner_epoch bigint NOT NULL,
    claimed_at timestamptz NOT NULL,
    renewed_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    released_at timestamptz,
    release_reason jsonb,
    verification jsonb,
    PRIMARY KEY (instance_id, run_id, gaggle, provider, external_id)
);

-- Each run and key's newest released incarnation seeds its record...
INSERT INTO control.claim_run_history
    (instance_id, run_id, gaggle, provider, external_id, workflow, owner_token, owner_epoch,
     claimed_at, renewed_at, expires_at, released_at, release_reason, verification)
SELECT DISTINCT ON (instance_id, run_id, gaggle, provider, external_id)
       instance_id, run_id, gaggle, provider, external_id, workflow, owner_token, owner_epoch,
       claimed_at, renewed_at, expires_at, released_at, release_reason, verification
FROM control.claim_history
ORDER BY instance_id, run_id, gaggle, provider, external_id, released_at DESC, owner_token DESC;

-- ...and a run's current lease supersedes it, as a ledger claim does.
INSERT INTO control.claim_run_history
    (instance_id, run_id, gaggle, provider, external_id, workflow, owner_token, owner_epoch,
     claimed_at, renewed_at, expires_at, released_at, release_reason, verification)
SELECT instance_id, run_id, gaggle, provider, external_id, workflow, owner_token, owner_epoch,
       claimed_at, renewed_at, expires_at, NULL, NULL, verification
FROM control.claim
ON CONFLICT (instance_id, run_id, gaggle, provider, external_id) DO UPDATE
SET workflow = excluded.workflow, owner_token = excluded.owner_token,
    owner_epoch = excluded.owner_epoch, claimed_at = excluded.claimed_at,
    renewed_at = excluded.renewed_at, expires_at = excluded.expires_at,
    released_at = NULL, release_reason = NULL, verification = excluded.verification;
