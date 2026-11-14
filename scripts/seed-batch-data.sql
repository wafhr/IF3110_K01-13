-- Benchmark dataset for regular and batch migrations: 500,000 accounts.
-- Run it on a target database registered in the platform:
--   psql "$TARGET_DATABASE_URL" -f scripts/seed-batch-data.sql
-- TestMigrationBatchBenchmarkIntegration loads this same file.
DROP TABLE IF EXISTS batch_benchmark_accounts;

CREATE TABLE batch_benchmark_accounts (
    id BIGINT PRIMARY KEY,
    balance_cents BIGINT NOT NULL,
    tier TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO batch_benchmark_accounts (id, balance_cents, tier)
SELECT id, (id * 37) % 1000000, CASE WHEN id % 10 = 0 THEN 'gold' ELSE 'standard' END
FROM generate_series(1, 500000) AS id;

ANALYZE batch_benchmark_accounts;
