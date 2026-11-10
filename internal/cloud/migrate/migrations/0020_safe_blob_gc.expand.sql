-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

ALTER TABLE control.blob_gc_candidate
    ADD COLUMN first_observed_at timestamptz,
    ADD COLUMN first_observed_etag text,
    ADD CONSTRAINT blob_gc_candidate_observation_pair
        CHECK (
            (first_observed_at IS NULL) =
            (first_observed_etag IS NULL)
        );

CREATE FUNCTION control.blob_column_referenced(
    table_name text,
    column_name text,
    candidate_digest text
) RETURNS boolean LANGUAGE plpgsql STABLE AS $$
DECLARE
    relation regclass;
    referenced boolean;
BEGIN
    relation := to_regclass(table_name);
    IF relation IS NULL THEN
        -- A missing reference class is an incompatible schema, not evidence
        -- that the digest is unreferenced. Retain every candidate fail-closed.
        RETURN true;
    END IF;
    EXECUTE format(
        'SELECT EXISTS (SELECT 1 FROM %s WHERE %I=$1)',
        relation,
        column_name
    ) INTO referenced USING candidate_digest;
    RETURN referenced;
END;
$$;

-- A writer class is registered only after every path that creates its
-- references participates in the storage mutation protocol. Until then its
-- ability to name an arbitrary digest makes every candidate unsafe.
CREATE TABLE control.blob_reference_writer_protocol (
    writer_class text PRIMARY KEY,
    protocol_version integer NOT NULL CHECK (protocol_version >= 1),
    qualified_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE FUNCTION control.blob_is_referenced(candidate_digest text)
RETURNS boolean LANGUAGE sql STABLE AS $$
    SELECT
        NOT EXISTS (
            SELECT 1 FROM control.blob_reference_writer_protocol
            WHERE writer_class='journal' AND protocol_version >= 1
        )
        OR NOT EXISTS (
            SELECT 1 FROM control.blob_reference_writer_protocol
            WHERE writer_class='recovery' AND protocol_version >= 1
        )
        OR NOT EXISTS (
            SELECT 1 FROM control.blob_reference_writer_protocol
            WHERE writer_class='child-custody' AND protocol_version >= 1
        )
        OR control.blob_column_referenced(
            'control.config_generation', 'blob_digest', candidate_digest
        )
        OR control.blob_column_referenced(
            'journal.run_artifact', 'digest', candidate_digest
        )
        OR control.blob_column_referenced(
            'control.recovery_record', 'patch_digest', candidate_digest
        )
        OR control.blob_column_referenced(
            'control.recovery_record', 'archive_digest', candidate_digest
        )
        OR control.blob_column_referenced(
            'control.child_lineage', 'actor_digest', candidate_digest
        )
        OR control.blob_column_referenced(
            'control.child_lineage', 'payload_digest', candidate_digest
        )
        OR control.blob_column_referenced(
            'control.child_lineage', 'proposal_digest', candidate_digest
        )
        OR control.blob_column_referenced(
            'control.child_lineage', 'snapshot_receipt_digest', candidate_digest
        )
        OR control.blob_column_referenced(
            'control.child_lineage', 'snapshot_bundle_digest', candidate_digest
        )
        OR control.blob_column_referenced(
            'control.child_lineage', 'result_receipt_digest', candidate_digest
        )
        OR control.blob_column_referenced(
            'control.child_lineage', 'result_bundle_digest', candidate_digest
        )
        OR control.blob_column_referenced(
            'control.child_lineage', 'result_ref', candidate_digest
        )
        OR control.blob_column_referenced(
            'control.child_lineage', 'workspace_ref', candidate_digest
        )
        OR control.blob_column_referenced(
            'control.child_authority', 'config_digest', candidate_digest
        )
        OR control.blob_column_referenced(
            'control.child_authority', 'policy_digest', candidate_digest
        )
        OR control.blob_column_referenced(
            'control.child_blob', 'digest', candidate_digest
        )
        OR control.blob_column_referenced(
            'control.child_blob_read', 'contract_digest', candidate_digest
        )
        OR control.blob_column_referenced(
            'control.child_blob_read', 'digest', candidate_digest
        )
        OR control.blob_column_referenced(
            'control.child_publication', 'intent_digest', candidate_digest
        )
        OR control.blob_column_referenced(
            'control.child_publication', 'receipt_digest', candidate_digest
        )
        OR control.blob_column_referenced(
            'control.child_disposition', 'request_digest', candidate_digest
        )
        OR control.blob_column_referenced(
            'control.child_disposition', 'previous_digest', candidate_digest
        )
        OR control.blob_column_referenced(
            'control.child_disposition', 'plan_digest', candidate_digest
        )
        OR control.blob_column_referenced(
            'control.child_disposition', 'result_ref', candidate_digest
        )
        OR control.blob_column_referenced(
            'control.child_disposition_history', 'request_digest', candidate_digest
        )
        OR control.blob_column_referenced(
            'control.child_disposition_history', 'previous_digest', candidate_digest
        )
        OR control.blob_column_referenced(
            'control.child_disposition_history', 'plan_digest', candidate_digest
        )
        OR control.blob_column_referenced(
            'control.child_disposition_history', 'result_ref', candidate_digest
        );
$$;
