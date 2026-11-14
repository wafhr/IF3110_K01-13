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
