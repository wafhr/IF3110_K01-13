-- Definisi tetap menyimpan identitas database dan pengguna ketika registrasi dihapus.
CREATE TABLE IF NOT EXISTS migration_definitions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    database_id UUID NOT NULL,
    name TEXT NOT NULL CHECK (octet_length(name) BETWEEN 1 AND 160 AND name = btrim(name)),
    current_revision BIGINT NOT NULL CHECK (current_revision > 0),
    created_by UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    frozen_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS migration_revisions (
    migration_id UUID NOT NULL REFERENCES migration_definitions(id),
    number BIGINT NOT NULL CHECK (number > 0),
    sql_up TEXT NOT NULL CHECK (octet_length(sql_up) BETWEEN 1 AND 1048576 AND sql_up ~ '[^[:space:]]'),
    sql_down TEXT NOT NULL DEFAULT '' CHECK (octet_length(sql_down) <= 1048576),
    irreversible BOOLEAN NOT NULL DEFAULT FALSE,
    irreversible_reason TEXT NOT NULL DEFAULT '',
    created_by UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (migration_id, number),
    CHECK (
        (irreversible AND sql_down = '' AND irreversible_reason ~ '[^[:space:]]') OR
        (NOT irreversible AND sql_down ~ '[^[:space:]]' AND irreversible_reason = '')
    )
);

ALTER TABLE migration_revisions ADD COLUMN IF NOT EXISTS batch_table TEXT NOT NULL DEFAULT '';
ALTER TABLE migration_revisions ADD COLUMN IF NOT EXISTS batch_key TEXT NOT NULL DEFAULT '';
ALTER TABLE migration_revisions ADD COLUMN IF NOT EXISTS batch_size INTEGER NOT NULL DEFAULT 0;

DO $$ BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'migration_revisions'::regclass AND conname = 'migration_revisions_batch_check'
    ) THEN
        ALTER TABLE migration_revisions ADD CONSTRAINT migration_revisions_batch_check CHECK (
            (batch_size = 0 AND batch_table = '' AND batch_key = '') OR
            (batch_size > 0 AND batch_table <> '' AND batch_key <> '')
        );
    END IF;
END $$;

-- Revisi pertama dan pembaruan penunjuk revisi terkini di-commit dalam satu transaksi.
DO $$ BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'migration_definitions'::regclass
          AND conname = 'migration_definitions_current_revision_fkey'
    ) THEN
        ALTER TABLE migration_definitions
        ADD CONSTRAINT migration_definitions_current_revision_fkey
        FOREIGN KEY (id, current_revision) REFERENCES migration_revisions(migration_id, number)
        DEFERRABLE INITIALLY DEFERRED;
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS migration_definitions_database_idx
    ON migration_definitions(database_id, created_at, id);

-- Perubahan draft menambahkan revisi baru agar salinan revisi sebelumnya tetap utuh.
CREATE OR REPLACE FUNCTION protect_migration_revision() RETURNS TRIGGER
LANGUAGE plpgsql AS $$ BEGIN
    RAISE EXCEPTION 'migration revisions are immutable'
        USING ERRCODE = '23514', CONSTRAINT = 'migration_revisions_immutable';
END $$;

DROP TRIGGER IF EXISTS migration_revisions_immutable ON migration_revisions;
CREATE TRIGGER migration_revisions_immutable
    BEFORE UPDATE OR DELETE ON migration_revisions
    FOR EACH ROW EXECUTE FUNCTION protect_migration_revision();

CREATE OR REPLACE FUNCTION protect_frozen_migration() RETURNS TRIGGER
LANGUAGE plpgsql AS $$ BEGIN
    IF OLD.frozen_at IS NOT NULL AND (
        NEW.frozen_at IS DISTINCT FROM OLD.frozen_at OR
        NEW.current_revision IS DISTINCT FROM OLD.current_revision
    ) THEN
        RAISE EXCEPTION 'executed migration definitions are frozen'
            USING ERRCODE = '23514', CONSTRAINT = 'migration_definitions_frozen';
    END IF;
    RETURN NEW;
END $$;

DROP TRIGGER IF EXISTS migration_definitions_frozen ON migration_definitions;
CREATE TRIGGER migration_definitions_frozen
    BEFORE UPDATE ON migration_definitions
    FOR EACH ROW EXECUTE FUNCTION protect_frozen_migration();
