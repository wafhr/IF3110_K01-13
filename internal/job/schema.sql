CREATE TABLE IF NOT EXISTS migration_jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    queue_position BIGINT GENERATED ALWAYS AS IDENTITY UNIQUE,
    execution_id UUID NOT NULL UNIQUE,
    database_id UUID NOT NULL,
    payload JSONB NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    operation TEXT NOT NULL DEFAULT 'execute' CHECK (operation IN ('execute', 'compensate')),
    touched BOOLEAN NOT NULL DEFAULT FALSE,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    committed_batches BIGINT NOT NULL DEFAULT 0 CHECK (committed_batches >= 0),
    committed_rows BIGINT NOT NULL DEFAULT 0 CHECK (committed_rows >= 0),
    total_rows BIGINT NOT NULL DEFAULT 0 CHECK (total_rows >= 0),
    last_error TEXT NOT NULL DEFAULT '',
    available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_owner TEXT,
    lease_expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT migration_jobs_status_check CHECK (status IN ('pending', 'running', 'partial', 'partially_reverted', 'succeeded', 'reverted', 'failed')),
    CONSTRAINT migration_jobs_lease_check CHECK ((status = 'running') = (lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS migration_jobs_runnable_idx ON migration_jobs (queue_position) WHERE status IN ('pending', 'running');
CREATE INDEX IF NOT EXISTS migration_jobs_database_idx ON migration_jobs (database_id, queue_position);

ALTER TABLE migration_jobs ADD COLUMN IF NOT EXISTS current_attempt INTEGER NOT NULL DEFAULT 0;
ALTER TABLE migration_jobs ADD COLUMN IF NOT EXISTS attempt_started_at TIMESTAMPTZ;

-- Every attempt of a job, successful or not, with the SQL it ran. Rows are
-- written by the queue transition that ends the attempt and never change.
CREATE TABLE IF NOT EXISTS migration_attempts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    sequence BIGINT GENERATED ALWAYS AS IDENTITY UNIQUE,
    job_id UUID NOT NULL REFERENCES migration_jobs(id),
    execution_id UUID NOT NULL,
    database_id UUID NOT NULL,
    migration_id UUID NOT NULL,
    name TEXT NOT NULL,
    revision BIGINT NOT NULL,
    action TEXT NOT NULL,
    operation TEXT NOT NULL,
    attempt INTEGER NOT NULL,
    actor_id UUID NOT NULL,
    sql TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('succeeded', 'reverted', 'failed', 'partial', 'partially_reverted', 'retrying', 'released', 'interrupted')),
    error TEXT NOT NULL DEFAULT '',
    committed_batches BIGINT NOT NULL DEFAULT 0,
    committed_rows BIGINT NOT NULL DEFAULT 0,
    started_at TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS migration_attempts_database_idx ON migration_attempts (database_id, sequence DESC);

CREATE OR REPLACE FUNCTION protect_migration_attempts() RETURNS TRIGGER
LANGUAGE plpgsql AS $$ BEGIN
    RAISE EXCEPTION 'migration attempts are append-only'
        USING ERRCODE = '23514', CONSTRAINT = 'migration_attempts_immutable';
END $$;

DROP TRIGGER IF EXISTS migration_attempts_immutable ON migration_attempts;
CREATE TRIGGER migration_attempts_immutable
    BEFORE UPDATE OR DELETE ON migration_attempts
    FOR EACH ROW EXECUTE FUNCTION protect_migration_attempts();

DROP TRIGGER IF EXISTS migration_attempts_truncate ON migration_attempts;
CREATE TRIGGER migration_attempts_truncate
    BEFORE TRUNCATE ON migration_attempts
    FOR EACH STATEMENT EXECUTE FUNCTION protect_migration_attempts();
