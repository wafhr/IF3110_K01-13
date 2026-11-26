CREATE TABLE IF NOT EXISTS platform_migration_executions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    sequence BIGINT GENERATED ALWAYS AS IDENTITY UNIQUE,
    database_id UUID NOT NULL,
    migration_id UUID,
    revision BIGINT,
    name TEXT NOT NULL CHECK (octet_length(name) BETWEEN 1 AND 160),
    action TEXT NOT NULL CHECK (action IN ('apply', 'rollback', 'forward')),
    sql TEXT NOT NULL CHECK (octet_length(sql) BETWEEN 1 AND 1048576 AND sql ~ '[^[:space:]]'),
    status TEXT NOT NULL DEFAULT 'success' CHECK (status = 'success'),
    executed_by UUID NOT NULL,
    started_at TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ,
    reverts_execution_id UUID UNIQUE,
    legacy_version BIGINT,
    UNIQUE (database_id, legacy_version),
    UNIQUE (id, database_id, migration_id, revision),
    FOREIGN KEY (reverts_execution_id, database_id, migration_id, revision)
        REFERENCES platform_migration_executions(id, database_id, migration_id, revision),
    CHECK (finished_at IS NULL OR finished_at >= started_at),
    CHECK ((action = 'rollback') = (reverts_execution_id IS NOT NULL)),
    CHECK (
        (legacy_version IS NULL AND migration_id IS NOT NULL AND revision IS NOT NULL AND revision > 0 AND finished_at IS NOT NULL) OR
        (legacy_version IS NOT NULL AND legacy_version > 0 AND migration_id IS NULL AND revision IS NULL AND action = 'apply')
    )
);

CREATE INDEX IF NOT EXISTS platform_migration_executions_database_idx
    ON platform_migration_executions(database_id, sequence DESC);

-- Pasangan rollback wajib merujuk eksekusi apply/forward dari database dan revisi yang sama.
CREATE OR REPLACE FUNCTION validate_migration_rollback_reference() RETURNS TRIGGER
LANGUAGE plpgsql AS $$ BEGIN
    IF NEW.action = 'rollback' AND NOT EXISTS (
        SELECT 1 FROM platform_migration_executions
        WHERE id = NEW.reverts_execution_id AND action IN ('apply', 'forward')
    ) THEN
        RAISE EXCEPTION 'rollback requires an applied execution'
            USING ERRCODE = '23514', CONSTRAINT = 'migration_rollback_reference';
    END IF;
    RETURN NEW;
END $$;

DROP TRIGGER IF EXISTS migration_rollback_reference ON platform_migration_executions;
CREATE TRIGGER migration_rollback_reference
    BEFORE INSERT ON platform_migration_executions
    FOR EACH ROW EXECUTE FUNCTION validate_migration_rollback_reference();

-- History yang berhasil di-commit dipertahankan sebagai catatan append-only.
CREATE OR REPLACE FUNCTION protect_migration_execution_history() RETURNS TRIGGER
LANGUAGE plpgsql AS $$ BEGIN
    RAISE EXCEPTION 'migration execution history is append-only'
        USING ERRCODE = '23514', CONSTRAINT = 'migration_execution_history_immutable';
END $$;

DROP TRIGGER IF EXISTS migration_execution_history_immutable ON platform_migration_executions;
CREATE TRIGGER migration_execution_history_immutable
    BEFORE UPDATE OR DELETE ON platform_migration_executions
    FOR EACH ROW EXECUTE FUNCTION protect_migration_execution_history();

DROP TRIGGER IF EXISTS migration_execution_history_truncate ON platform_migration_executions;
CREATE TRIGGER migration_execution_history_truncate
    BEFORE TRUNCATE ON platform_migration_executions
    FOR EACH STATEMENT EXECUTE FUNCTION protect_migration_execution_history();
