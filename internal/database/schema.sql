CREATE TABLE IF NOT EXISTS managed_databases (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL UNIQUE CHECK (char_length(name) BETWEEN 1 AND 100 AND name = btrim(name)),
    host TEXT NOT NULL CHECK (char_length(host) BETWEEN 1 AND 253),
    port INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535),
    database_name TEXT NOT NULL CHECK (char_length(database_name) BETWEEN 1 AND 63),
    username TEXT NOT NULL CHECK (char_length(username) BETWEEN 1 AND 63),
    password_ciphertext BYTEA NOT NULL,
    ssl_mode TEXT NOT NULL CHECK (ssl_mode IN ('disable', 'allow', 'prefer', 'require', 'verify-ca', 'verify-full')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
