-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

-- Claim history matches the file ledger's (localscheduler.ClaimLedger): one
-- record per run and key, written whenever a lease is taken, renewed or verified, and
-- completed on release. A live or expired-and-overwritten claim therefore has
-- an unreleased record, so the release columns become nullable.
ALTER TABLE control.claim_history
    ALTER COLUMN released_at DROP NOT NULL,
    ALTER COLUMN release_reason DROP NOT NULL;

-- Keep only each run and key's newest released incarnation before the key
-- becomes unique.
DELETE FROM control.claim_history AS older
USING control.claim_history AS newer
WHERE older.instance_id = newer.instance_id
  AND older.run_id = newer.run_id
  AND older.gaggle = newer.gaggle
  AND older.provider = newer.provider
  AND older.external_id = newer.external_id
  AND (older.released_at, older.owner_token) < (newer.released_at, newer.owner_token);

CREATE UNIQUE INDEX claim_history_run_key
    ON control.claim_history (instance_id, run_id, gaggle, provider, external_id);

-- A run's current lease supersedes its earlier released record, as a ledger
-- claim does.
INSERT INTO control.claim_history
    (instance_id, gaggle, provider, external_id, run_id, workflow, owner_token, owner_epoch,
     claimed_at, renewed_at, expires_at, released_at, release_reason, verification)
SELECT instance_id, gaggle, provider, external_id, run_id, workflow, owner_token, owner_epoch,
       claimed_at, renewed_at, expires_at, NULL, NULL, verification
FROM control.claim
ON CONFLICT (instance_id, run_id, gaggle, provider, external_id) DO UPDATE
SET workflow = excluded.workflow, owner_token = excluded.owner_token,
    owner_epoch = excluded.owner_epoch, claimed_at = excluded.claimed_at,
    renewed_at = excluded.renewed_at, expires_at = excluded.expires_at,
    released_at = NULL, release_reason = NULL, verification = excluded.verification;
