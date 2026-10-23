-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

CREATE TABLE control.surrender (
    instance_id text NOT NULL,
    run_id text NOT NULL,
    stage text NOT NULL,
    attempt integer NOT NULL,
    doc bytea NOT NULL,
    recovery_ack boolean NOT NULL DEFAULT false,
    PRIMARY KEY (instance_id, run_id, stage, attempt)
);

CREATE TABLE control.config_generation (
    instance_id text NOT NULL,
    digest text NOT NULL,
    blob_digest text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (instance_id, digest)
);

CREATE TABLE control.config_generation_pin (
    instance_id text NOT NULL,
    digest text NOT NULL,
    owner text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (instance_id, digest, owner),
    FOREIGN KEY (instance_id, digest)
        REFERENCES control.config_generation (instance_id, digest)
        ON DELETE RESTRICT
);

CREATE TABLE control.applied_config (
    instance_id text PRIMARY KEY,
    digest text NOT NULL,
    generation bigint NOT NULL,
    applied_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (instance_id, digest)
        REFERENCES control.config_generation (instance_id, digest)
        ON DELETE RESTRICT
);

CREATE TABLE control.blob_gc_candidate (
    digest text PRIMARY KEY,
    unreferenced_at timestamptz NOT NULL,
    delete_started_at timestamptz,
    intent_id text,
    intent_etag text
);
