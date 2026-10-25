CREATE TABLE IF NOT EXISTS access_users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    is_platform_admin BOOLEAN NOT NULL DEFAULT FALSE,
    is_active BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS access_groups (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS access_group_members (
    group_id UUID NOT NULL REFERENCES access_groups(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES access_users(id) ON DELETE CASCADE,
    PRIMARY KEY (group_id, user_id)
);

CREATE TABLE IF NOT EXISTS access_group_database_permissions (
    group_id UUID NOT NULL REFERENCES access_groups(id) ON DELETE CASCADE,
    database_id UUID NOT NULL,
    permission TEXT NOT NULL CHECK (permission IN ('view_only', 'view_edit')),
    PRIMARY KEY (group_id, database_id)
);

CREATE TABLE IF NOT EXISTS access_sessions (
    token_hash BYTEA PRIMARY KEY,
    previous_token_hash BYTEA UNIQUE,
    previous_expires_at TIMESTAMPTZ,
    current_token_during_grace TEXT,
    current_token_grace_expires_at TIMESTAMPTZ,
    csrf_token TEXT NOT NULL,
    user_id UUID NOT NULL REFERENCES access_users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL,
    rotated_at TIMESTAMPTZ NOT NULL,
    absolute_expires_at TIMESTAMPTZ NOT NULL
);

ALTER TABLE access_sessions ADD COLUMN IF NOT EXISTS current_token_during_grace TEXT;
ALTER TABLE access_sessions ADD COLUMN IF NOT EXISTS current_token_grace_expires_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS access_sessions_user_id_idx ON access_sessions(user_id);
CREATE INDEX IF NOT EXISTS access_group_members_user_id_idx ON access_group_members(user_id);
CREATE INDEX IF NOT EXISTS access_permissions_database_id_idx ON access_group_database_permissions(database_id);
