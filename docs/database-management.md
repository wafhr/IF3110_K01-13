# Database management

The platform uses PostgreSQL to store users, groups, sessions, grants, and the
connection registry. Admins can register PostgreSQL or MySQL 8.4 targets.

## Schema ownership

Each module owns and initializes its schema.

- `internal/access/schema.sql` creates the user, group, membership, grant, and
  session tables. The `internal/access` package embeds and executes this file.
- `internal/database/schema.sql` creates the `managed_databases` registry
  table. The `internal/database` package embeds and executes this file.
- Legacy migration history uses `platform_migrations` in each target database.
  The `internal/migration` package creates this table when legacy history is
  first read or written.

The access-control and registry schema files define separate tables and have
separate owners and initialization routines. Each module embeds its local
schema. A shared versioned schema runner would require explicit schema
versions and an application order.

Grants store registry UUIDs. The access-control module validates the database
registration when creating a grant. Deleting a registration removes its grants
and revokes affected sessions in one transaction. Changes to the registry or
access-control module must preserve this integration.

## Connection configuration

Set `PLATFORM_DATABASE_URL` to the platform database connection string and
`DATABASE_ENCRYPTION_KEY` to 32 random bytes encoded as base64. Generate a key
in PowerShell with the following command.

```powershell
[Convert]::ToBase64String([Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
```

The registry encrypts target connection passwords using AES-256-GCM. Persist
the key in a secret manager. Losing or replacing it prevents the application
from decrypting saved connection passwords. Registry responses exclude target
passwords.

On the first startup with an empty platform database, the application creates
a Platform Admin using `BOOTSTRAP_ADMIN_USERNAME` and
`BOOTSTRAP_ADMIN_PASSWORD`. The bootstrap password must contain at least 12
characters. Bootstrap values apply when the initial account is created.

Copy `.env.example` to `.env` in the repository root and enter local settings
in `.env`. The server automatically loads `.env` before reading configuration.
`.env.example` serves as the configuration template. Environment variables
already set by the shell or deployment take precedence. Configuration can also
come entirely from process environment variables when `.env` is absent.
Invalid or unreadable files produce an error that excludes their contents.
Git ignores `.env`. Keep personal passwords and encryption keys in local or
deployment configuration. Set `APP_ENV=production` to enable secure session
cookies. When TLS terminates at a reverse proxy, also set `COOKIE_SECURE=true`.

## Target registry

Platform Admins register connections at `/manage/databases`. The following
management endpoints are available.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/admin/databases` | List registered connections |
| `POST` | `/admin/databases` | Register a connection |
| `GET` | `/admin/databases/{databaseID}` | Read connection metadata |
| `PUT` | `/admin/databases/{databaseID}` | Replace connection metadata and credentials |
| `DELETE` | `/admin/databases/{databaseID}` | Delete a registration and its grants |
| `POST` | `/admin/databases/test-connection` | Test settings before saving them |
| `POST` | `/admin/databases/{databaseID}/test-connection` | Test a saved connection |

Endpoints accept `name`, `dbms`, `host`, `port`, `database_name`, `username`,
`password`, and `ssl_mode` in the JSON request body. `dbms` accepts `postgres`
or `mysql`. Omitting it selects PostgreSQL for compatibility with existing
registrations. The default port is `5432` for PostgreSQL and `3306` for MySQL.
The default SSL mode is `require`. PostgreSQL supports `disable`, `allow`,
`prefer`, `require`, `verify-ca`, and `verify-full`. MySQL supports `disable`,
`prefer`, and `require`. Management operations require a Platform Admin.
State-changing requests also require the session CSRF token. Updating or
deleting a registration invalidates its cached connection pool.

After registration, the admin grants a group access to the database UUID.
Users see databases granted through their groups. Effective permission rules
are described in the access-control guide. [Access control](access-control.md).

## Viewer

Viewer endpoints require a valid session and at least `view_only` permission.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/databases` | List databases accessible to the user |
| `GET` | `/databases/{databaseID}/schemas` | List visible schemas |
| `GET` | `/databases/{databaseID}/schemas/{schema}/tables` | List tables in a schema |
| `GET` | `/databases/{databaseID}/schemas/{schema}/tables/{table}` | Read column, constraint, and index details |
| `GET` | `/databases/{databaseID}/schemas/{schema}/tables/{table}/data` | Read a page of table data |

The data endpoint accepts `limit`, which defaults to 50 and has a maximum of
200, and `offset`, which defaults to 0. Values are returned as text or `null`
to preserve numeric precision. For MySQL, the selected database is displayed
as the schema and binary values use hexadecimal representation. The target
account needs `USAGE` on the PostgreSQL schema or access to the MySQL database,
and `SELECT` on the requested data.

Connection, target authentication, and target permission failures produce
sanitized responses that exclude connection credentials.

## Integration test

The supplied Docker configuration provides disposable PostgreSQL and MySQL
servers for integration tests.

```powershell
docker compose -f compose.test.yaml up -d --wait
$env:TEST_DATABASE_URL = 'postgres://if3110_test_admin:if3110-integration-password@localhost:55432/postgres?sslmode=disable'
$env:TEST_MYSQL_DSN = 'if3110_test_admin:if3110-mysql-test-password@tcp(127.0.0.1:53306)/if3110_mysql_test?tls=false'

go test -tags=integration ./tests

docker compose -f compose.test.yaml down -v
```

Tests create temporary PostgreSQL platform and target databases and a target
role with the `if3110_it_` prefix. MySQL tests create tables with the same
prefix in the disposable `if3110_mysql_test` database, then remove those tables
and the migration history table. The PostgreSQL account needs permission to
create and drop databases and roles. The MySQL account needs permission to
create and drop tables in the test database. Use `TEST_DATABASE_URL` with a
dedicated disposable server.
