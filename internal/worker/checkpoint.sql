CREATE TABLE IF NOT EXISTS {{schema}}.platform_migration_batch_scopes (
    execution_id UUID PRIMARY KEY,
    database_id UUID NOT NULL,
    migration_id UUID NOT NULL,
    revision BIGINT NOT NULL CHECK (revision > 0),
    batch_table TEXT NOT NULL,
    batch_key TEXT NOT NULL,
    batch_size INTEGER NOT NULL CHECK (batch_size > 0),
    scope_end TEXT,
    total_rows BIGINT NOT NULL CHECK (total_rows >= 0),
    status TEXT NOT NULL DEFAULT 'running' CHECK (status IN ('running', 'completed', 'reverted')),
    started_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    CHECK ((status = 'running') = (completed_at IS NULL))
);

CREATE TABLE IF NOT EXISTS {{schema}}.platform_migration_batches (
    execution_id UUID NOT NULL REFERENCES {{schema}}.platform_migration_batch_scopes(execution_id),
    batch_number INTEGER NOT NULL CHECK (batch_number > 0),
    first_key TEXT NOT NULL,
    last_key TEXT NOT NULL,
    row_count BIGINT NOT NULL CHECK (row_count > 0),
    committed_at TIMESTAMPTZ NOT NULL,
    reverted_at TIMESTAMPTZ,
    PRIMARY KEY (execution_id, batch_number),
    CHECK (reverted_at IS NULL OR reverted_at >= committed_at)
);
