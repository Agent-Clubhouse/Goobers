-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

-- Recovery records index retained snapshots whose bundles live in object
-- storage. A run may retain several snapshots. The identity is immutable;
-- retention and the owning epoch only move forward. identity_unix_ns keeps the
-- record's capture time exactly, since timestamptz rounds to microseconds.
CREATE TABLE control.recovery_record (
    instance_id text NOT NULL,
    repository_key text NOT NULL,
    run_id text NOT NULL,
    snapshot_sha text NOT NULL,
    version integer NOT NULL,
    ref text NOT NULL,
    base_ref text NOT NULL,
    base_sha text NOT NULL,
    patch_digest text NOT NULL,
    archive_digest text NOT NULL,
    archive_bytes bigint NOT NULL CHECK (archive_bytes > 0),
    archive_format text NOT NULL CHECK (archive_format IN ('', 'full', 'delta')),
    bundle_key text NOT NULL CHECK (bundle_key <> ''),
    identity_unix_ns bigint NOT NULL,
    indexed_at timestamptz NOT NULL DEFAULT now(),
    retain_until timestamptz NOT NULL,
    owner_epoch bigint NOT NULL CHECK (owner_epoch > 0),
    CHECK (retain_until > to_timestamp(identity_unix_ns / 1e9)),
    PRIMARY KEY (instance_id, repository_key, run_id, snapshot_sha),
    -- A bundle belongs to exactly one record, so deleting a reaped record's
    -- bundle can never strand another record.
    CONSTRAINT recovery_record_bundle_unique UNIQUE (instance_id, bundle_key)
);

-- Bundles handed back by a reap and not yet confirmed deleted. Publish refuses
-- these keys, so a re-publication cannot adopt a blob its reaper is deleting.
CREATE TABLE control.recovery_bundle_tombstone (
    instance_id text NOT NULL,
    bundle_key text NOT NULL,
    reaped_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (instance_id, bundle_key)
);

CREATE INDEX recovery_record_retention ON control.recovery_record (instance_id, retain_until);

CREATE FUNCTION control.recovery_record_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.instance_id, NEW.repository_key, NEW.run_id, NEW.snapshot_sha, NEW.version,
        NEW.ref, NEW.base_ref, NEW.base_sha, NEW.patch_digest, NEW.archive_digest,
        NEW.archive_bytes, NEW.archive_format, NEW.bundle_key, NEW.identity_unix_ns,
        NEW.indexed_at)
       IS DISTINCT FROM
       (OLD.instance_id, OLD.repository_key, OLD.run_id, OLD.snapshot_sha, OLD.version,
        OLD.ref, OLD.base_ref, OLD.base_sha, OLD.patch_digest, OLD.archive_digest,
        OLD.archive_bytes, OLD.archive_format, OLD.bundle_key, OLD.identity_unix_ns,
        OLD.indexed_at) THEN
        RAISE EXCEPTION 'recovery record identity is immutable' USING ERRCODE = 'check_violation';
    END IF;
    NEW.retain_until := GREATEST(OLD.retain_until, NEW.retain_until);
    NEW.owner_epoch := GREATEST(OLD.owner_epoch, NEW.owner_epoch);
    RETURN NEW;
END;
$$;

CREATE TRIGGER recovery_record_guard BEFORE UPDATE ON control.recovery_record
    FOR EACH ROW EXECUTE FUNCTION control.recovery_record_guard();

-- Cancel receipts bind a key to one request. An unfinished receipt is
-- uncertain and is never pruned; a recorded result is written once.
CREATE TABLE control.cancel_receipt (
    instance_id text NOT NULL,
    key text NOT NULL CHECK (octet_length(key) BETWEEN 1 AND 200),
    actor text NOT NULL CHECK (octet_length(actor) <= 1024),
    payload bytea NOT NULL CHECK (octet_length(payload) BETWEEN 1 AND 16384),
    result bytea CHECK (octet_length(result) BETWEEN 1 AND 16384),
    owner_epoch bigint NOT NULL CHECK (owner_epoch > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    CHECK ((result IS NULL) = (finished_at IS NULL)),
    PRIMARY KEY (instance_id, key)
);

CREATE INDEX cancel_receipt_finished ON control.cancel_receipt (instance_id, finished_at)
    WHERE finished_at IS NOT NULL;

CREATE FUNCTION control.cancel_receipt_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.instance_id, NEW.key, NEW.actor, NEW.payload, NEW.created_at)
       IS DISTINCT FROM (OLD.instance_id, OLD.key, OLD.actor, OLD.payload, OLD.created_at)
       OR (OLD.result IS NOT NULL AND (NEW.result, NEW.finished_at) IS DISTINCT FROM (OLD.result, OLD.finished_at))
       OR NEW.owner_epoch < OLD.owner_epoch THEN
        RAISE EXCEPTION 'cancel receipt transition is not monotonic' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER cancel_receipt_guard BEFORE UPDATE ON control.cancel_receipt
    FOR EACH ROW EXECUTE FUNCTION control.cancel_receipt_guard();

-- The terminal-report outbox keeps one row per provider projection. Each
-- admission of a different intent takes a new generation from a sequence, so
-- a generation (and the key/generation idempotency key) is never reused, even
-- after a delivered row is pruned. A delivery lease is measured on the
-- database clock and is only acknowledged by its token.
CREATE SEQUENCE control.terminal_report_generation AS bigint;

CREATE TABLE control.terminal_report (
    instance_id text NOT NULL,
    effect_key text NOT NULL,
    effect bytea NOT NULL,
    report_identity text NOT NULL,
    generation bigint NOT NULL CHECK (generation > 0),
    delivered_generation bigint NOT NULL DEFAULT 0,
    body text,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    retry_at timestamptz NOT NULL DEFAULT '-infinity',
    lease_token uuid,
    lease_until timestamptz,
    owner_epoch bigint NOT NULL CHECK (owner_epoch > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    delivered_at timestamptz,
    CHECK ((lease_token IS NULL) = (lease_until IS NULL)),
    CHECK (delivered_generation <= generation),
    CHECK ((delivered_at IS NULL) OR (delivered_generation = generation)),
    PRIMARY KEY (instance_id, effect_key)
);

CREATE INDEX terminal_report_due ON control.terminal_report (instance_id, retry_at)
    WHERE delivered_at IS NULL;

CREATE INDEX terminal_report_delivered ON control.terminal_report (instance_id, delivered_at)
    WHERE delivered_at IS NOT NULL;

CREATE FUNCTION control.terminal_report_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.instance_id, NEW.effect_key, NEW.created_at) IS DISTINCT FROM (OLD.instance_id, OLD.effect_key, OLD.created_at)
       OR NEW.generation < OLD.generation
       OR NEW.delivered_generation < OLD.delivered_generation
       OR NEW.owner_epoch < OLD.owner_epoch THEN
        RAISE EXCEPTION 'terminal report transition is not monotonic' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER terminal_report_guard BEFORE UPDATE ON control.terminal_report
    FOR EACH ROW EXECUTE FUNCTION control.terminal_report_guard();
